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
promise build file.pr        # Compile to executable
promise run file.pr          # Build and run
promise test file.pr         # Run @test functions
promise check file.pr        # Type-check only
promise ast file.pr          # Print the AST
promise exec '<code>'        # Execute inline code
echo '<code>' | promise      # Execute from stdin
promise install              # Install to ~/.promise/
promise init                 # Create new promise.mod (planned)
promise add <url>            # Add dependency (planned)
promise remove <url>         # Remove dependency (planned)
promise update               # Update dependencies (planned)
promise fmt                  # Format source code (planned)
promise lock                 # Regenerate lockfile (planned)
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
  string name;
  int age;
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
  col.Vector[int] list = col.Vector[int]()
}
```

The identifier before the URL is **mandatory** and is the only way to reference that module's exports in the file.

**Design rationale:** Each source file is self-contained — you can read a `.pr` file and understand every reference without consulting other files. The full URL next to the alias makes it immediately clear what `io.println` or `col.Vector` refers to. This optimizes for reading and understanding code locally, which is the most common operation.

### 4.4 Visibility

All declarations are **public** by default. Use `` `public `` meta annotation to explicitly mark something as public (reserved for future use when private-by-default is introduced).

For now, everything is public. In a future revision, declarations will be **private by default**, and `` `public `` will be required to export them.

---

## 5. Type System

### 5.1 Primitive Types — Defined as Regular Types

Promise does **not** have a separate namespace for primitive types. Instead, primitives are defined as regular `type` declarations annotated with `` `native ``, where the compiler backend provides the underlying representation and operator implementations. This means `int`, `f64`, `bool`, etc. are all types in the standard library, not compiler magic.

#### Raw Fields

A field annotated with `` `raw `` uses an LLVM type identifier directly as its type instead of a Promise type. LLVM type identifiers (`i1`, `i8`, `i16`, `i32`, `i64`, `float`, `double`, etc.) are valid identifiers in Promise and can appear as the type of a `` `raw `` field.

`` `raw `` is only supported on field definitions. Everywhere else — variable declarations, function parameters, return types — you must use a Promise-defined type. This keeps the type system simple: only field definitions need special handling for raw values.

#### Standard Library Primitive Definitions

```promise
type int `native {
  +(int other) int `native;
  -(int other) int `native;
  *(int other) int `native;
  /(int other) int `native;
  %(int other) int `native;
  ==(int other) bool `native;
  !=(int other) bool `native;
  <(int other) bool `native;
  >(int other) bool `native;
  <=(int other) bool `native;
  >=(int other) bool `native;
  -() int `native;                // unary negation
}

type i8 `native { /* same pattern */ }
type i16 `native { /* same pattern */ }
type i32 `native { /* same pattern */ }
type i64 `native { /* same pattern */ }

type uint `native { /* same pattern */ }
type u8 `native { /* same pattern */ }
type u16 `native { /* same pattern */ }
type u32 `native { /* same pattern */ }
type u64 `native { /* same pattern */ }

type f32 `native { /* same pattern */ }
type f64 `native { /* same pattern */ }

type bool `native {
  &&(bool other) bool `native;
  ||(bool other) bool `native;
  ==(bool other) bool `native;
  !=(bool other) bool `native;
  !() bool `native;
}
```

Types annotated with `` `native `` have their underlying representation provided by the compiler backend — there are no user-visible fields. The `` `native `` annotation on methods means the runtime/compiler backend provides the implementation directly. This unifies the type system — there is no distinction between "primitive" and "user-defined" types.

#### Operator Overloading

Operators are ordinary methods whose name is the operator symbol. The compiler does **not** generate any built-in operator code — all operators for all types (including primitives) are defined as methods in the standard library.

Methods marked `` `native `` have no Promise body; the runtime/compiler backend provides the implementation directly (e.g. mapping `int.+` to an LLVM `add` instruction).

Any user-defined type can define operators the same way:

```promise
type Vec2 {
  f64 x `value;
  f64 y `value;

  +(Vec2 other) Vec2 {
    return Vec2(x: this.x + other.x, y: this.y + other.y);
  }

  ==(Vec2 other) bool {
    return this.x == other.x && this.y == other.y;
  }
}
```

Supported operator method names: `+`, `-`, `*`, `/`, `%`, `==`, `!=`, `<`, `>`, `<=`, `>=`. Unary operators use the same symbol with no parameters (e.g. `-() int` for negation). Both unary and binary forms can coexist on the same type — the compiler disambiguates by argument count.

#### Operator Precedence

Operator precedence is fixed by the language and cannot be overridden by user-defined types. From highest to lowest:

| Precedence | Operators | Associativity |
|-----------|-----------|---------------|
| 1 (highest) | `.` `()` `[]` | Left |
| 2 | Unary `-` `!` | Right (prefix) |
| 3 | `*` `/` `%` | Left |
| 4 | `+` `-` | Left |
| 5 | `..` `..=` | Non-associative |
| 6 | `<` `>` `<=` `>=` | Non-associative |
| 7 | `==` `!=` | Non-associative |
| 8 | `&&` | Left |
| 9 | `\|\|` | Left |
| 10 | `?.` `?:` | Left |
| 11 (lowest) | `=` `+=` `-=` `*=` `/=` | Right |

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

A **view** is the perspective through which a value is accessed via a particular type in its inheritance chain. When a `Dog` value is passed where `Animal` is expected, the `Animal` view of `Dog` is used — the vtable pointer is set to the `Animal`-layout vtable generated for `Dog`, exposing only `Animal`'s fields and methods.

#### Diagram

```
┌──────────────────────────────────────────────────────────────────┐
│  T#t  (1 per source declaration, compile-time generated)         │
│  - name: "Vector"                                                │
│  - generic_params: [E]                                           │
│  - fields: [...]                                                 │
│  - meta: [...]                                                   │
│  - parent_type: nullable                                         │
│  - `type fields live here                                        │
│                                                                  │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │ T#m[int]  (1 per monomorphization, compile-time gen)       │  │
│  │ - resolved_params: {E: int}                                │  │
│  │ - type_ptr: → T#t ◀───────────────────────────────────── │  │
│  │ - `variant fields live here                                │  │
│  └────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────┐
│ T#i (1 per live object, heap-allocated)                       │
│ - variant_ptr: → T#m[int]                                     │
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
  string name;
  int age;
  speak() string `abstract;
}
```

The compiler generates:

```
Animal_vtable = {
  [0] get_name:  fn(Animal#v) -> string
  [1] set_name:  fn(Animal#v, string)
  [2] get_age:   fn(Animal#v) -> int
  [3] set_age:   fn(Animal#v, int)
  [4] speak:     fn(Animal#v) -> string
}
```

At the call site, `animal.name` compiles to `vtable[0](animal_value)` and `animal.speak()` compiles to `vtable[4](animal_value)`. The call site does not know or care whether `name` is a stored field or a computed getter — the vtable function handles it.

**Slot assignment:** Slots are assigned in **declaration order** within each inheritance level, with parent slots always forming a **prefix**. When a child extends a parent, the parent's slots occupy the first N positions (unchanged), and the child's new fields and methods are appended after. This ensures that a parent's vtable layout is a prefix of every child's vtable layout — maximizing vtable reuse across types that share the same parent. Multiple types inheriting from the same parent share identical slot positions for the parent's fields and methods.

**Why fields go through the vtable:**

This decouples the call site from the concrete implementation. A parent type can declare a field `string name`, and a child type can satisfy it with either a stored field or a computed getter — the call site code is identical in both cases. This is what enables interfaces to declare data fields that concrete types can implement however they choose (see Section 5.4).

**Getter/setter syntactic sugar:**

Since all field access goes through vtable getter/setter calls, the language provides syntactic sugar so that field access looks like direct member access, not function calls. The dot operator (`.`) on a field name desugars to the corresponding vtable getter or setter:

```promise
// Reading — dot access desugars to getter call
animal.name                       // → vtable.get_name(animal_value)
player.position.x                 // → vtable.get_x(vtable.get_position(player_value))

// Writing — assignment through dot desugars to setter call
animal.name = "Rex";              // → vtable.set_name(animal_value, "Rex")

// Compound assignment — desugars to get + operator + set
player.health += 10;              // → vtable.set_health(pv, vtable.get_health(pv) + 10)
player.score *= 2;                // → vtable.set_score(pv, vtable.get_score(pv) * 2)

// Chained access
game.world.player.health += 1;   // each dot is a getter; final assignment is a setter
```

This sugar is essential because without it, code would require explicit getter/setter calls (`animal.getName()`, `animal.setName("Rex")`), and operators on fields would be impossible — you cannot write `animal.getName() += 10`. The dot syntax makes vtable-dispatched access indistinguishable from direct field access at the call site.

**Inheritance extends the vtable:**

```promise
type Dog is Animal {
  string breed;
  speak() string { return "Woof!"; }
}
```

Dog's vtable **starts with** Animal's layout and appends new slots:

```
Dog_vtable = {
  // Animal slots (same positions — prefix-compatible)
  [0] get_name:  fn(Dog#v) -> string      → Dog's name field getter
  [1] set_name:  fn(Dog#v, string)        → Dog's name field setter
  [2] get_age:   fn(Dog#v) -> int         → Dog's age field getter
  [3] set_age:   fn(Dog#v, int)           → Dog's age field setter
  [4] speak:     fn(Dog#v) -> string      → Dog.speak

  // Dog-specific slots (appended)
  [5] get_breed: fn(Dog#v) -> string      → Dog's breed field getter
  [6] set_breed: fn(Dog#v, string)        → Dog's breed field setter
}
```

For single inheritance, the child's vtable is prefix-compatible with the parent's — a Dog vtable pointer works wherever an Animal vtable pointer is expected, because slots 0–4 are at the same positions.

**Multiple inheritance — per-view vtables:**

When a type has multiple parents, the compiler generates a **separate vtable for each parent view**. This is necessary because different parents have different slot layouts.

```promise
type Named {
  string name;
  greet() string { return "Hi, I'm {this.name}"; }
}

type Audible {
  volume() int `abstract;
  speak() string `abstract;
}

type Dog is Named, Audible {
  string breed;
  int loudness;

  speak() string { return "Woof!"; }
  volume() int { return this.loudness; }
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
  f64 x;
  f64 y;
  distanceTo(Positioned &other) f64 {
    dx := this.x - other.x;
    dy := this.y - other.y;
    return math.sqrt(dx * dx + dy * dy);
  }
}

type Player is Positioned {
  // Stores x, y as real fields — vtable getters read from instance memory
  f64 x;
  f64 y;
  string name;
}

type CameraTarget is Positioned {
  // No stored x, y — computed from tracked entity
  Entity tracked;

  x() f64 { return this.tracked.position().x; }
  y() f64 { return this.tracked.position().y; }
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
  string name;
  int health;

  // Value field — lives in T#v, copied with the value struct
  f64 x `value;
  f64 y `value;

  // Variant field — shared across all instances of this monomorphization
  string spritePath `variant;

  // Type field — shared across all instances of this type declaration
  string typeName `type;
}
```

- **Instance fields** (default): per-object, heap-allocated. Each instance has its own copy.
- **`` `value `` fields**: live in the value struct. Copied every time the value is passed or assigned. Best for small, frequently-accessed data (coordinates, flags).
- **`` `variant `` fields**: shared across all instances of the same generic monomorphization. Useful for per-specialization metadata (e.g., a sprite path shared by all `Player[Warrior]` instances). Mutable only at initialization.
- **`` `type `` fields**: shared across all instances of the type declaration regardless of generic parameters. Useful for reflection metadata. Mutable only at initialization.

#### Primitives in the Four-Struct Model

Since primitives are regular types, `int` works like any other type in the four-struct model:

- `int#v` = `{ vtable*, int#i*, i64 }` — vtable pointer, instance pointer, and the raw i64 value
- `int#i` = `{ int#m* }` — pointer to variant (no pointer back to value)
- `int#m` = `{ int#t* }` — pointer to type (compile-time generated)
- `int#t` = `{ metadata }` — name, reflection info (compile-time generated)

The `int` vtable contains getter/setter for the raw value plus all operator methods (`+`, `-`, `==`, etc.). For performance, the compiler optimizes away unnecessary indirection for primitives (e.g., `int` on the stack is just an `i64` in practice, with the vtable/instance/variant/type pointers elided when not needed).

### 5.3 Variable Declarations

Variable declarations use **type-first** syntax (Dart/C++ style):

```promise
int x = 42;
string name = "Alice";
f64 pi = 3.14159;
int[] numbers = [1, 2, 3];
map[string, int] scores = {"alice": 100, "bob": 85};
```

Type inference with `:=`:

```promise
x := 42;                // inferred as int
name := "Alice";        // inferred as string
```

### 5.4 Inheritance

A type declares its parent types with `is`. There is no distinction between inheritance and interface implementation — both use the same keyword. An interface is simply a type whose methods are all `` `abstract `` — it uses the same `is` keyword and the same vtable machinery.

```promise
type Shape {
  f64 x;
  f64 y;

  area() f64 `abstract;
}

type Circle is Shape {
  f64 radius;

  area() f64 { return 3.14159 * this.radius * this.radius; }
}

type Drawable {
  draw(Canvas &canvas) `abstract;
}

type Circle is Shape, Drawable {
  f64 radius;

  area() f64 { ... }

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
- **Default method implementations**: a parent type can provide method bodies. These become concrete function pointers in the child's vtable. The child can override them by providing its own implementation, which replaces the function pointer in the vtable slot. `` `abstract `` and a method body are **mutually exclusive**: `` `abstract `` means no body and the child must override; a body means a default implementation that the child may optionally override.
- **Field placement inheritance**: when a child inherits from a parent, field placement annotations (`` `value ``, `` `variant ``, `` `type ``) are inherited as declared. If the parent declares `f64 x `value`, the child's value struct also contains `x`.

#### Structural Interface Satisfaction

By default, a type must declare `is Interface` to be assignable to an interface type. This keeps intent explicit and prevents accidental satisfaction.

For lightweight, widely-satisfied interfaces (e.g. `Printable`, `Hashable`), the `` `structural `` meta tag on the interface enables **structural satisfaction**: any type that provides concrete implementations for all of the interface's abstract methods — with matching signatures — is automatically assignable to that interface, even without `is`.

```promise
type Printable `structural {
  toString() string `abstract;
}

type Point {
  int x;
  int y;

  // No `is Printable` — but satisfies Printable structurally
  toString() string { return "(" + this.x.toString() + ", " + this.y.toString() + ")"; }
}

Printable p = Point(x: 1, y: 2);  // OK — Point has toString() with matching signature
p.toString();                       // dispatches to Point.toString via view-specific vtable
```

Structural satisfaction requires an **exact signature match**: parameter types, return type, and error capability must all be identical. A method with the same name but different parameters does not satisfy the interface.

When a value crosses a type boundary through structural satisfaction (or through a second+ parent), the compiler emits a **view-specific vtable** ordered by the target interface's slot layout. The value struct's vtable pointer is swapped to this view vtable at the coercion point (variable declaration, assignment, function argument, or return statement).

### 5.5 Generics

Generics use **square brackets** `[]`. Constraints are expressed inline in the type parameter list.

```promise
type map[K: Hashable + Eq, V] {
  Bucket[K, V][] buckets;

  get(K &key) V&? `instance { ... }
  set(K key, V value) `instance { ... }
}

sort[T: Ord](T[] ~list) {
  ...
}
```

#### Standard Constraint Interfaces

The standard library provides these interfaces for use as generic constraints:

```promise
type Eq {
  ==(Self &other) bool `abstract;
  !=(Self &other) bool { return !(this == other); }
}

type Ord is Eq {
  <(Self &other) bool `abstract;
  >(Self &other) bool { return other < this; }
  <=(Self &other) bool { return !(this > other); }
  >=(Self &other) bool { return !(this < other); }
}

type Hashable {
  hash() int `abstract;
}
```

All primitive types (`int`, `f64`, `string`, `bool`, etc.) implement `Eq` and `Ord`. `string` and `int` also implement `Hashable`. User-defined types can implement these interfaces to participate in generic algorithms like `sort`, `map` key lookup, and stream combinators like `distinct()`, `min()`, and `max()`.

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
  Custom(u8 r, u8 g, u8 b),
}
```

### 5.7 Constructors

Promise supports implicit constructors (auto-generated from fields) and explicit `new` constructors (user-defined with validation). Factory constructors provide named alternative construction paths. The `new`/`drop` pair forms a symmetric constructor/destructor lifecycle.

#### Implicit Constructors

For simple types with no validation needs, the compiler generates a constructor from field declarations. All arguments are named:

```promise
type User {
  string name;           // required — no default, not optional
  int age;               // required
  string? bio;           // optional — T? defaults to none
  int score = 0;         // optional — has default
}

User(name: "Alice", age: 30)              // OK: bio=none, score=0
User(name: "Alice", age: 30, bio: "hi")   // OK
User(name: "Alice")                       // ERROR: missing required field 'age'
```

A field is **required** if it is not `T?` and does not have `= default`. All required fields must be provided. Default expressions are evaluated at the call site each time the argument is omitted (see Section 9.3).

#### `Self` Type Alias

`Self` is a compiler-supported type alias meaning "the enclosing type with all generic parameters as-is." After monomorphization, `Self` resolves to the concrete type.

| Context | `Self` resolves to |
|---------|-------------------|
| Inside `type Foo { ... }` | `Foo` |
| Inside `type Box[T] { ... }` | `Box[T]` |
| After monomorphization for `Box[int]` | `Box[int]` |
| Inside `type Dog is Animal { ... }` | `Dog` (not `Animal`) |

`Self` is capitalized because it is a type name, contrasting with `this` which is a value. `Self` is usable in return types, constructor calls, parameter types, and field types within type bodies:

```promise
type Point {
  f64 x `final;
  f64 y `final;

  offset(f64 dx, f64 dy) Self {
    return Self(x: this.x + dx, y: this.y + dy);
  }
}
```

#### `` `final `` Fields

A field annotated `` `final `` can be assigned during construction but is frozen afterward. The compiler generates a vtable getter but **no setter**.

```promise
type Token {
  string raw `final;
  int line `final;
  int col `final;
}

main() {
  t := Token(raw: "if", line: 1, col: 0);
  string s = t.raw;       // OK — getter works
  t.raw = "else";         // ERROR: cannot assign to `final field 'raw'
}
```

Write access rules:
- **`new` body**: can assign `` `final `` fields
- **`` `factory `` body**: can assign `` `final `` fields on locally-created instances only (variables initialized from constructor calls within the factory; parameter or external instances cannot have their `` `final `` fields modified)
- **Everywhere else**: assignment is a compile error

Additional rules:
- Child types cannot shadow a `` `final `` field with a mutable one
- `` `final `` fields can have defaults: `int version \`final = 1;`
- `` `final `` + `T?` is valid: `string? tag \`final;` (defaults to `none`, frozen after)
- Defining a custom setter on a `` `final `` field name is a compile error
- Defining a custom getter on a `` `final `` field name is allowed (overrides the generated getter)
- `` `final `` is orthogonal to placement — `` `final \`value ``, `` `final \`variant ``, `` `final \`type `` are all valid
- `` `copy `` types can have `` `final `` fields — bitwise copies get the same frozen values

#### Explicit `new` Constructor

When a type needs validation or computed initialization, define a `new` method. It **replaces** the implicit constructor.

```promise
type Percentage {
  int value `final;

  new(int value) {
    if value < 0 { this.value = 0; }
    else if value > 100 { this.value = 100; }
    else { this.value = value; }
  }
}

Percentage(value: 120)    // calls new(value:), clamps to 100
Percentage(value: 50)     // calls new(value:), stores 50
```

Semantics:
1. Compiler allocates instance (zero-initialized) and stores RTTI
2. `new` body executes with implicit `~this` (mutable access to fresh instance)
3. Instance returned as value struct

Rules:
- Receiver is implicitly `~this` — not written in the signature
- Return type is implicitly `Self` — not written in the signature
- Call site syntax is unchanged: `Type(args...)` — arguments match `new`'s parameter names (not field names)
- When `new` is defined, the implicit constructor is gone — all construction goes through `new`
- Every `` `final `` field must be definitely assigned on all code paths through `new`
- Non-`` `final `` fields not assigned in `new` remain zero-initialized (or use their default)

Parameters can differ from field names, enabling computed construction:

```promise
type Point {
  f64 x `final;
  f64 y `final;

  // Polar constructor — param names differ from field names
  new(f64 radius, f64 angle) {
    this.x = radius * cos(angle);
    this.y = radius * sin(angle);
  }
}

Point(radius: 5.0, angle: 1.57)    // named after new() params
```

#### Failable `new`

Append `!` to make `new` failable. The caller must handle the error using standard error handling (see Section 7).

```promise
type Port {
  int value `final;

  new(int value)! {
    if value < 1 || value > 65535 {
      raise InvalidArgError(msg: "invalid port number");
    }
    this.value = value;
  }
}

Port(value: 80)!       // calls new(value:), validates, OK
Port(value: -1)!       // calls new(value:), raises InvalidArgError
```

A failable constructor integrates with standard error handling — auto-propagation in `!` functions, explicit `?` handling, or `!` to assert-and-panic (see Section 7):

```promise
serve(int portNum) Server! {
  Port p = Port(value: portNum);   // auto-propagates InvalidArgError on failure
  return Server(port: p);
}
```

When `new` raises, the instance is never returned — no `drop()` runs on the incomplete instance. Fields assigned before the `raise` that hold non-copy values are leaked. Validation should precede field assignment to minimize this.

#### Factory Constructors

A factory is a method annotated `` `factory ``. It provides named alternative construction paths with special privileges:

- Can modify `` `final `` fields on locally-created instances
- Can return child types (return type is `Self` or a type that `is Self`)
- Can be failable (`!`)

`` `factory `` implies `` `variant `` placement — per-monomorphization, all generics resolved. This is necessary because a factory on `Box[T]` must know which `T` to create. A factory has **no `this` receiver**.

```promise
type Color {
  int r `final;
  int g `final;
  int b `final;

  red() Self `factory {
    return Self(r: 255, g: 0, b: 0);
  }

  hex(string code) Self! `factory {
    if code.len != 7 || code[0] != '#' {
      raise ParseError(msg: "invalid hex color");
    }
    c := Self(r: 0, g: 0, b: 0);
    c.r = parseHex(code[1:3]);     // OK — `factory can set `final
    c.g = parseHex(code[3:5]);
    c.b = parseHex(code[5:7]);
    return c;
  }
}

Color c1 = Color.red();
Color c2 = Color.hex("#FF8800")!;
```

Factories can return child types:

```promise
type Shape `abstract {
  string color `final;

  new(string color) {
    this.color = color;
  }

  circle(string color, f64 r) Self `factory {
    return Circle(color: color, radius: r);
  }
}

Shape s = Shape.circle("red", 5.0);   // returns Circle typed as Shape
```

If the type defines `new`, `Self(...)` / `Type(...)` inside the factory routes through `new` — validation always runs.

#### Inheritance and Constructors

When a parent type defines `new`, the child **must** also define `new` and call `super(...)` to initialize the parent.

```promise
type Animal {
  string name `final;
  int age;

  new(string name, int age) {
    this.name = name;
    this.age = age;
  }
}

type Dog is Animal {
  string breed `final;

  new(string name, int age, string breed) {
    super(name, age);       // calls Animal.new
    this.breed = breed;
  }
}

Dog(name: "Rex", age: 3, breed: "Lab")
```

When the parent has only the implicit constructor, `super(field: value, ...)` uses field-name syntax:

```promise
type Dog is Animal {
  string breed `final;

  new(string name, string breed) {
    super(name: name, age: 0);    // field-name syntax for implicit parent constructor
    this.breed = breed;
  }
}
```

**Relaxed `super()` rule** — `super()` does NOT need to be the first statement. Instead, two invariants apply:

1. **No `this` access before `super()`** — cannot read or write any field (own or inherited) until the parent is initialized
2. **All code paths must call `super()` exactly once** — definite-call analysis (same infrastructure as `` `final `` field definite-assignment)

This enables validation before parent construction, conditional super calls, and computed arguments:

```promise
type SecureConn is Connection {
  string cert `final;

  new(string rawUrl, string cert)! {
    // Validation before super — no this access, just params + locals
    string normalized = normalizeUrl(rawUrl);
    if !isValid(normalized) {
      raise InvalidUrlError(url: rawUrl);
    }
    super(url: normalized, timeout: 30);
    this.cert = cert;
  }
}
```

```promise
type Logger is Output {
  new(string target)! {
    if target == "stdout" {
      super(stream: stdout);
    } else if target == "stderr" {
      super(stream: stderr);
    } else {
      Stream f = openFile(target);
      super(stream: f);
    }
    // all branches called super — OK
  }
}
```

Compile errors for `super()`:

| Rule | Error |
|------|-------|
| `this` access before `super()` on that path | "cannot access 'this' before calling super()" |
| Path exits `new()` without calling `super()` | "all code paths must call super()" |
| `super()` called twice on same path | "super() already called on this path" |
| `super()` inside a loop | "super() cannot be called inside a loop" |
| Child omits `new()` when parent has `new()` | "type Child must define new() because parent Parent defines new()" |
| Parent `new()` is failable but child `new()` is not | "new() on Child must be failable because parent Parent has failable new()" |

Abstract types can define `new` to enforce initialization contracts — children call `super(...)` to satisfy them. Abstract types themselves still cannot be instantiated directly.

#### Construction Lifecycle

```
allocate + zero-init + RTTI
  → super() [if parent has new]
  → new() body [if has new]
  → `final fields frozen
  → instance returned
  → ... use ...
  → drop() [if has drop, at scope exit]
  → field drops [compiler-inserted, reverse order]
  → free
```

#### Definite-Assignment Analysis

The compiler uses flow-sensitive analysis (shared infrastructure) for three checks:
1. Every `` `final `` field is assigned on all paths through `new`
2. `super()` is called on all paths (exactly once, no loops)
3. No `this` access before `super()` on any path

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
process(string &data) {              // shared borrow
  io.println(data);
}

modify(string ~data) {               // mutable borrow
  data.append(" world");
}

consume(string data) {               // takes ownership
  // data is dropped at end of scope
}

main() {
  string s = string("hello");
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
first(string &a, string &b) string& { return a; }  // inferred: output borrows from a
name(&this) string& { return this.name; }           // inferred: output borrows from this

// Rare case: compiler cannot determine which input the output borrows from.
// Explicit annotation required:
longest['a](string &'a a, string &'a b) string &'a {
  if a.len() > b.len() { return a; }
  return b;
}
```

### 6.4 Copy and Clone

`` `copy `` and `` `clone `` are built-in meta annotations that control assignment semantics:

```promise
// Bitwise copy — compiler verifies all fields are also `copy
type Point `copy {
  f64 x `value;
  f64 y `value;
}

// Auto-generated deep clone — compiler generates clone() Self method
type Document `clone {
  string title;
  string[] pages;
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
readFile(string &path) string! {
  // On success:
  return contents;

  // On failure:
  raise io.FileNotFoundError(path);
}
```

The `!` suffix on the return type means: "this function returns `(string, Error)`".

### 7.2 Calling Failable Functions

In a **failable function** (return type has `!`), a naked call to another failable function **auto-propagates** the error — if the callee fails, the caller immediately returns the error to its own caller. This is the most common case and requires no extra syntax:

```promise
process() string! {
  string content = readFile("data.txt");    // auto-propagates on error
  return content.trim();
}
```

The explicit `?` suffix is allowed for self-documentation but has the same effect:

```promise
  string content = readFile("data.txt")?;   // same as above — explicit propagation
```

In a **non-failable function**, calling a failable function without handling is a **compile-time error** — there is nowhere to propagate to. The caller must handle the error with `?` or unwrap with `!`:

```promise
main() {
  // Handle with ? — block must provide recovery value or diverge (return/panic)
  string content = readFile("data.txt") ? e {
    io.println("Failed: {e.message()}");
    return;
  };

  // Handle with ? — error value not needed
  string content = readFile("data.txt") ? {
    return;
  };

  // Unwrap (panics on error — for prototyping only)
  string content = readFile("data.txt")!;
}
```

#### Error handler syntax

The `? e { ... }` form handles errors inline. The error binding is optional:

| Form | Meaning |
|------|---------|
| `expr ? e { ... }` | Handle error, bind error value to `e` |
| `expr ? { ... }` | Handle error, discard error value |

The handler block must either produce a **recovery value** of the expected type, or **diverge** (`return`, `break`, `panic`). If it produces a value, that value is used in place of the failed call:

```promise
// Recovery value
string content = readFile("data.txt") ? { "" };    // use empty string on failure

// Diverge
string content = readFile("data.txt") ? e {
  io.println("Error: {e.message()}");
  return;
};
```

#### Capturing the raw result

To inspect both the value and error without propagation, destructure into a tuple:

```promise
(content, err) := readFile("data.txt");
if err is present {
  io.println("Failed: {err.message()}");
} else {
  io.println(content);
}
```

#### Summary

| Call form | Behavior | Context |
|-----------|----------|---------|
| `foo()` | Auto-propagate error | `!` function only |
| `foo()?` | Explicit propagate (same as naked) | `!` function only |
| `foo() ? e { ... }` | Handle error, bind to `e` | Any function |
| `foo() ? { ... }` | Handle error, discard error value | Any function |
| `foo()!` | Panic on error | Any function |
| `(val, err) := foo()` | Capture raw result | Any function |

**Note:** Auto-propagation does not cross lambda boundaries. Inside a non-`!` lambda, failable calls must be handled explicitly with `?` or `!`.

### 7.3 Error Types

```promise
type Error {
  message() string `abstract;
}

type FileNotFoundError is Error {
  string path;

  message() string {
    return "file not found: {this.path}";
  }
}
```

### 7.4 `raise` Statement

`raise` is used to return an error from a `!`-function. It is **not** an exception — it is sugar for returning the error half of the result struct.

```promise
divide(f64 a, f64 b) f64! {
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
- **Fields**: `string name `meta;`
- **Methods**: `greet() string `meta { ... }`
- **Functions**: `add(int a, int b) int `meta { ... }`
- **Parameters**: `foo(int x `doc("description"), int y `deprecated) { ... }`

### 8.2 Examples

```promise
type OldThing `serializable `version(2) `deprecated(since: "1.3", message: "Use newMethod instead") {
  string name `json(name: "user_name") `required;
  int age `json(name: "user_age");
}

fastAdd(int a, int b) int `inline {
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
| `` `copy ``  | types           | Bitwise copy on assignment; compiler verifies all fields are also `` `copy `` |
| `` `clone `` | types           | Auto-generate `clone() Self` method (deep copy)   |
| `` `required ``| fields         | Field must be present during deserialization; validation error otherwise |
| `` `final `` | fields          | Immutable after construction; can only be set in `new` or `` `factory `` body (see Section 5.7) |
| `` `factory ``| methods        | Factory constructor with `` `variant `` placement; no `this`, returns `Self` or child (see Section 5.7) |
| `` `doc ``   | any, parameters | AST-attached documentation (see Section 8.4)      |

User-defined metas are available through the type system at compile time for meta-programming and code generation.

### 8.4 Documentation (`` `doc ``)

`` `doc `` attaches documentation directly to the AST node. Unlike comments, `` `doc `` is preserved in the parsed tree, unambiguously bound to its declaration, and available to tooling, IDE support, and AI agents at compile time.

```promise
type HttpClient `doc("HTTP client with connection pooling and automatic retry.") {
  int maxRetries `doc("Maximum number of retry attempts before failing.");
  Duration timeout `doc("Per-request timeout.");

  get(~this, string url `doc("The URL to fetch.")) Response!
      `doc("Perform a GET request. Returns the response or an error.") `instance {
    ...
  }
}

divide(f64 a `doc("dividend"), f64 b `doc("divisor"))
    f64! `doc("Divide a by b. Raises MathError on division by zero.") {
  if b == 0.0 {
    raise MathError("division by zero");
  }
  return a / b;
}
```

The parameter is a plain string. Tooling can extract structured sections (parameters, return values, errors) from the text by convention, but the language itself treats it as an opaque string. This keeps the meta simple while giving AI agents a reliable, parseable documentation source that is always in sync with the code structure.

Parameter-level `` `doc `` annotations are placed after the parameter name (and before any default value), providing per-parameter documentation that is structurally bound to each parameter rather than embedded in a free-text block.

---

## 9. Functions & Methods

Functions are declared without a keyword — the name, parameter list, and optional return type are sufficient to identify a function declaration. This follows the Dart/C++/Java convention.

### 9.1 Free Functions

```promise
greet(string &name) string {
  return "Hello, {name}!";
}
```

### 9.2 Methods

Methods are defined inside the type body and correspond to the four struct levels. The method's level determines what `this` refers to and which fields are accessible.

#### Value Methods (default)

By default, `this` is the **value struct**, copied when the method is called. Value methods can access `` `value `` fields only.

```promise
type Point {
  f64 x `value;
  f64 y `value;

  distanceTo(this, Point other) f64 {
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
  int value;

  increment(~this) `instance {
    this.value += 1;
  }

  current(&this) int `instance {
    return this.value;
  }
}
```

#### Variant Methods (`` `variant ``)

Variant methods receive the **variant struct**. One method copy exists per monomorphization. They can access `` `variant `` fields only.

```promise
type Collection[T] {
  string typeName `variant;

  describeType() string `variant {
    return "Collection of {this.typeName}";
  }
}
```

#### Type Methods (`` `type ``)

Type methods are effectively **namespaced functions**. No `this` is passed at call time — the type struct is static and known at compile time. They can access `` `type `` fields only. These replace the `static` keyword.

```promise
type Counter {
  int value;

  create() Counter `type {
    return Counter(value: 0);
  }
}

// Called as:
Counter c = Counter.create();
```

**Note:** The name `new` is reserved for explicit constructors (see Section 5.7). Type methods that create instances should use descriptive names like `create`, `from`, or `empty`. Factory constructors (`` `factory ``) use `` `variant `` placement instead of `` `type `` — see Section 5.7.

### 9.3 Named Arguments, Defaults & Optional Parameters

#### Definition Syntax

Any parameter can have a **default value** with `= expression`. Parameters whose type uses the `T?` sugar are **optional** — when omitted at the call site, the function receives `none`.

```promise
sendEmail(
    string to,                    // required
    string subject,               // required
    string body = "",             // has default — skippable
    string? cc,                   // optional — skippable, receives none
    int priority = 3              // has default — skippable
) bool! {
  // cc is Option[string] — test with: if cc { ... } (see Section 14.1)
  ...
}
```

There is no ordering constraint on required, defaulted, and optional parameters in the definition — any order is valid. The `this`/`&this`/`~this` receiver is unaffected: it is never named, never defaulted, and always implicit.

Parameters can carry meta annotations (see Section 8), placed after the parameter name and before any default value:

```promise
connect(string host `doc("hostname"), int port `doc("port number") = 8080) Connection! { ... }
```

**`T?` vs `Option[T]`:** Only the `T?` sugar triggers skippability. If a parameter is declared with `Option[T]` explicitly, it is a required parameter of optional type — the caller must provide it.

#### Call-Site Syntax

At any call site, an argument can be passed by name using `name: expression`, where `name` matches a parameter name from the definition.

**Positional arguments must precede all named arguments.** Once a named argument appears, all subsequent arguments must also be named. Named arguments can appear in any order.

```promise
// All positional
sendEmail("bob@ex.com", "Hi");

// Positional then named (skip optional params)
sendEmail("bob@ex.com", "Hi", priority: 1);

// All named (any order)
sendEmail(to: "bob@ex.com", subject: "Hi", cc: "a@ex.com");
sendEmail(cc: "a@ex.com", subject: "Hi", to: "bob@ex.com");

// ERROR — named before positional:
sendEmail(cc: "a@ex.com", "bob@ex.com", "Hi");

// ERROR — interleaving:
sendEmail("bob@ex.com", cc: "a@ex.com", "Hi");
```

Valid pattern: `[positional...] [named...]` — positional arguments fill parameters left-to-right in declaration order, named arguments fill parameters by name.

#### Argument Matching

The compiler resolves arguments in this order:

1. **Validate structure**: scan the argument list and verify all positional arguments come before any named argument. If a positional argument appears after a named argument, emit a compile error.
2. **Match positional arguments**: fill parameters left-to-right in declaration order. The first positional argument fills the first parameter, the second fills the second, and so on.
3. **Match named arguments**: resolve each named argument by parameter name. Mark each matched parameter as filled. Error on unknown names, duplicate names, or naming a parameter already filled by a positional argument.
4. **Fill defaults and optionals**: for each unfilled parameter — if it has a default value, insert the default expression; if its type is `T?`, insert `none`; otherwise emit a "missing required argument" error.

#### Skipping Parameters

A parameter can be omitted at the call site if it has a default value or its type is `T?`. When using positional arguments, you cannot skip a parameter and provide a later one positionally — you must use named arguments to target specific parameters while skipping others:

```promise
// Skip body and cc, provide priority by name:
sendEmail("bob@ex.com", "Hi", priority: 1);

// Skip body, provide cc by name:
sendEmail("bob@ex.com", "Hi", cc: "alice@ex.com");
```

#### Default Value Evaluation

Default value expressions are evaluated **at the call site** each time the argument is omitted. This avoids the "mutable default" problem — each call gets a fresh evaluation. Default expressions can reference literals, constants, pure function calls, and module-level values. They **cannot** reference sibling parameters from the same function call.

```promise
// VALID defaults:
connect(string host, int port = 8080) Connection! { ... }
createId(string prefix, string id = Uuid.generate()) Thing { ... }

// INVALID — referencing sibling parameter:
range(int start, int end = start + 10) { ... }  // compile error
```

#### Constructor Defaults

Constructor parameters mirror field declarations. Fields with `= expression` defaults become optional constructor parameters (the default is evaluated at the call site each time the argument is omitted). Fields with `T?` type become optional constructor parameters (defaulting to `none`). Fields without defaults and without `T?` type are **required** — omitting them is a compile error.

```promise
type Config {
  string host;           // required — must be provided
  int port = 8080;       // optional — default evaluated at call site
  string? logFile;       // optional — defaults to none
}

Config(host: "localhost");                          // OK: port=8080, logFile=none
Config(host: "localhost", logFile: "/var/log/app"); // OK: port=8080
Config(host: "localhost", port: 9090);              // OK: logFile=none
Config(port: 9090);                                 // ERROR: missing required field 'host'
```

When a type defines an explicit `new` constructor, the implicit constructor is replaced — call site arguments match `new`'s parameter names instead of field names. See Section 5.7 for full constructor design including `new`, failable constructors, factories, `` `final `` fields, and inheritance.

### 9.4 Lambdas / Closures

```promise
add := |int a, int b| -> int { return a + b; };
doubled := list.map(|x| x * 2);
```

Lambdas support default parameter values:

```promise
greet := |string name, string greeting = "Hello"| -> string {
  return "{greeting}, {name}!";
};
greet("Alice");            // greeting uses default
greet("Alice", "Hi");      // greeting = "Hi"
```

Closures capture variables from enclosing scopes. `Copy` types (primitives, refs, types with `copy` meta) are automatically captured by copy. Non-`Copy` types require the `move` keyword, which transfers ownership into the closure:

```promise
string greeting = "hello";
// move required: string is non-Copy
closure := move |string name| -> string {
  return "{greeting}, {name}";
};
// greeting is no longer usable here — it was moved into the closure

int x = 42;
// no move needed: int is Copy
adder := |int y| -> x + y;
// x is still usable here — it was copied into the closure
```

Nested lambdas can capture from grandparent scopes — intermediate lambdas automatically capture forwarded variables:

```promise
int x = 10;
f := |int a| -> int {
  g := |int b| -> x + b;  // x captured through f
  return g(a);
};
```

Named arguments are **not available** when calling through a function-type variable, because function types erase parameter names (see Section 9.5). Named arguments only work when calling a known function or lambda directly.

### 9.5 Function Types

Function types use arrow syntax instead of a keyword:

```promise
(int, int) -> int                  // function taking two ints, returning int
(string&) -> bool                  // function taking a borrowed string, returning bool
() -> ()                           // function taking nothing, returning nothing
```

Function types **erase parameter names and default values**. Only the types and borrow modifiers are part of the function type signature. This means named arguments and default-value skipping are only available when calling a function by its declared name — not when calling through a function-type variable:

```promise
add(int a, int b) int { return a + b; }

add(a: 1, b: 2);          // VALID: calling by name, names available
(int, int) -> int fn = add;
fn(1, 2);                  // VALID: positional through function-type variable
fn(a: 1, b: 2);            // ERROR: function type has no parameter names
```

### 9.6 No Function/Method Overloading

Promise does not support **function or method overloading** — defining multiple functions or methods with the same name but different parameter signatures. Each function name within a scope must be unique.

```promise
// NOT ALLOWED — same name, different signatures:
parse(string s) Document { ... }
parse(Bytes b) Document { ... }   // compile error: 'parse' already defined

// Use distinct names instead:
parseString(string s) Document { ... }
parseBytes(Bytes b) Document { ... }
```

**Rationale:** Overloading interacts poorly with type inference (`:=`), first-class functions (which overload does `callback = parse` refer to?), and error-type resolution. Default parameters and optional parameters (`T?`) already cover the common "fewer arguments" use case, while generics and interfaces handle the "different types" case. Keeping dispatch unambiguous simplifies both the compiler and the developer's mental model.

Note that **operator overloading** (defining `+`, `==`, etc. as methods inside a type body) is supported — see Section 5.7. This is not name-based overloading; each operator symbol has exactly one definition per type.

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

`match` tests a value against a series of patterns. Arms are checked top-to-bottom; the first match executes. The compiler verifies **exhaustiveness** — all possible cases must be covered (use `_` as a catch-all).

#### Literal patterns

```promise
label := match status {
  200 => "OK",
  404 => "Not Found",
  _ => "Unknown",
};
```

#### Enum destructuring

```promise
match color {
  Color.Red => io.println("red"),
  Color.Green => io.println("green"),
  Color.Custom(r, g, b) => io.println("rgb({r},{g},{b})"),
  _ => io.println("other"),
}
```

#### Type patterns

Match by runtime type. The binding variable is scoped to the arm:

```promise
match shape {
  Circle c => math.pi * c.radius * c.radius,
  Rect r => r.width * r.height,
  _ => 0.0,
}
```

A type pattern without binding just checks the type:

```promise
match animal {
  Dog => io.println("it's a dog"),
  Cat => io.println("it's a cat"),
  _ => io.println("something else"),
}
```

#### Guards

Add `if` after a pattern for an additional condition:

```promise
match n {
  x if x > 0 => "positive",
  0 => "zero",
  _ => "negative",
}
```

Guards can combine with type patterns:

```promise
match animal {
  Dog d if d.age > 10 => "old dog",
  Dog d => "young dog",
  Cat => "cat",
  _ => "other",
}
```

#### Match rules

- **Exhaustive**: every possible value must be covered. Omitting `_` when cases are incomplete is a compile error.
- **First match wins**: arms are checked top-to-bottom.
- **Expression or statement**: `match` can appear as an expression (returns a value) or a statement. When used as an expression, all arms must produce the same type.
- **Bindings are scoped**: a binding like `d` in `Dog d =>` is only valid in that arm's body.
- **No fallthrough**: each arm is independent. There is no implicit fallthrough between arms.

### 10.3 Type Checking with `is` and Casting with `as`

#### `is` — runtime type check with narrowing

The `is` keyword tests whether a value is an instance of a given type. Inside the `if` block, the compiler **narrows** the variable to the checked type:

```promise
Animal animal = getDog();

if animal is Dog {
  animal.bark();              // animal is narrowed to Dog here
}
```

This follows the same narrowing pattern as optional truthiness (`if cc { ... }` narrows `T?` to `T`). The `else` branch does **not** narrow — `animal` remains `Animal`.

`is` works in any boolean context:

```promise
if animal is Dog && animal.age > 5 {
  // animal is Dog here — narrowed before the && guard
}

bool isDog = animal is Dog;   // no narrowing — just a bool test
```

#### `as` — type casting

`as` performs a safe cast, returning an optional. `as!` performs an unsafe cast that panics on failure:

```promise
Dog? dog = animal as Dog;     // safe — returns none if animal is not a Dog
Dog dog = animal as! Dog;     // unsafe — panics if animal is not a Dog
```

`as` composes with optional chaining:

```promise
animal as Dog ?: defaultDog;            // cast or default
(animal as Dog)?.bark();                // cast and chain
```

#### `is` keyword disambiguation

The `is` keyword appears in three contexts. The parser disambiguates by syntactic position:

| Context | Example | How it's recognized |
|---------|---------|---------------------|
| Type declaration | `type Dog is Animal` | After `type IDENT` |
| Optional pattern | `x is present` / `x is absent` | `present`/`absent` are contextual keywords after `is` |
| Type check | `animal is Dog` | Expression context, IDENT after `is` is a type name |

These never conflict — type declarations and expression patterns occupy different syntactic positions.

### 10.4 Loops

```promise
// While
while condition {
  // ...
}

// While with unwrap binding — loops while the expression is not none,
// binding the unwrapped value each iteration (see Section 14.1)
while item := optionalExpr {
  // item is the unwrapped T, not T?
}

// For-in — iterates any stream[T] (see Section 12)
for item in collection {
  // ...
}

// For-in with index — desugars to enumerate() (see Section 12.2)
for i, item in collection {
  // ...
}

// For-in with range
for i in 0..10 {
  // i takes values 0, 1, 2, ..., 9 (exclusive end)
}

for i in 0..=10 {
  // i takes values 0, 1, 2, ..., 10 (inclusive end)
}

// Infinite loop (no condition — Go style)
for {
  if done { break; }
}

// Classic for
for int i = 0; i < 10; i += 1 {
  // ...
}
```

The `for item in expr` loop works on any value whose type implements `stream[T]` — it calls `expr.iter()` to obtain an `iter[T]`, then loops calling `next()` until `none` is returned. See Section 12 for the full iteration protocol.

---

## 11. strings & Interpolation

strings are UTF-8 encoded, owned, and heap-allocated.

```promise
string name = "world";
string msg = "hello, {name}!";           // string interpolation with {}
string raw = r"no \n escape here";        // raw string
string multi = """
  multi-line
  string literal
""";
```

string slices (`&str` equivalent) use `string&` for borrowed string data.

---

## 12. Streams, Ranges & Generators

Promise provides a unified iteration and streaming abstraction through two core interfaces: `stream[T]` (a reusable factory that produces cursors) and `iter[T]` (a single-pass cursor that yields elements one at a time). Because Promise has no function coloring and uses goroutine-based transparent I/O, a single `stream[T]` type handles both synchronous and asynchronous data sources — a generator that performs I/O simply suspends its goroutine during the blocking operation, with no change to its type signature.

### 12.1 Core Interfaces

```promise
type iter[T] {
  next() T? `abstract;
}
```

`iter[T]` is the stateful cursor. Each call to `next()` returns the next element wrapped in `T?`, or `none` when the sequence is exhausted. This combines the traditional `hasNext()` + `next()` two-method pattern into a single call, leveraging Promise's optional type system.

```promise
type stream[T] {
  iter() iter[T] `abstract;

  // Intermediate combinators — lazy, return a new stream
  map[R]((T) -> R transform) stream[R] { ... }
  filter((T) -> bool predicate) stream[T] { ... }
  flatMap[R]((T) -> stream[R] transform) stream[R] { ... }
  take(int count) stream[T] { ... }
  skip(int count) stream[T] { ... }
  takeWhile((T) -> bool predicate) stream[T] { ... }
  skipWhile((T) -> bool predicate) stream[T] { ... }
  zip[U](stream[U] other) stream[(T, U)] { ... }
  chain(stream[T] other) stream[T] { ... }
  enumerate() stream[(int, T)] { ... }
  scan[R](R initial, (R, T) -> R accumulate) stream[R] { ... }
  chunk(int size) stream[T[]] { ... }
  distinct() stream[T] { ... }

  // Terminal operations — eager, consume the stream
  fold[R](R initial, (R, T) -> R accumulate) R { ... }
  reduce((T, T) -> T combine) T? { ... }
  collect() T[] { ... }
  count() int { ... }
  any((T) -> bool predicate) bool { ... }
  every((T) -> bool predicate) bool { ... }
  contains(T value) bool { ... }
  first() T? { ... }
  last() T? { ... }
  find((T) -> bool predicate) T? { ... }
  min() T? { ... }
  max() T? { ... }
  forEach((T) action) { ... }
  join(string separator = "") string { ... }
}
```

**Key design properties:**

- **`stream[T]` is a factory.** Each call to `iter()` produces a fresh, independent cursor. This means streams are reusable — you can iterate the same stream multiple times and get the same elements each time (for deterministic sources).
- **`iter[T]` is a single-pass cursor.** Once `next()` returns `none`, the iterator is exhausted. There is no `reset()`.
- **Intermediate operations are lazy.** Calling `stream.map(fn)` does not execute `fn` — it returns a new `stream[T]` that applies `fn` on demand when iterated. Multiple intermediate operations compose into a single pass over the data.
- **Terminal operations are eager.** Calling `stream.collect()` or `stream.count()` consumes the stream and produces a result.
- **Constraint-dependent combinators**: `distinct()` requires `T: Eq`. `min()` and `max()` require `T: Ord`. These constraints are enforced at the call site via generic bounds.

### 12.2 For-in Desugaring

The `for item in expr` loop desugars into `stream[T]` and `iter[T]` operations:

```promise
for item in stream { body }

// Desugars to:
{
  iter[T] _iter = stream.iter();
  while item := _iter.next() {
    body
  }
}
```

This uses `while ... :=` unwrap binding (see Section 10.3): the loop continues as long as `_iter.next()` returns a value, binding the unwrapped element to `item` each iteration.

The indexed form desugars through `enumerate()`:

```promise
for i, item in collection { body }

// Desugars to:
for (i, item) in collection.enumerate() { body }
```

### 12.3 Ranges

The `..` operator constructs a `range` value. `..` produces a half-open (exclusive end) range; `..=` produces an inclusive range:

```promise
0..10       // 0, 1, 2, ..., 9     (exclusive end — half-open)
0..=10      // 0, 1, 2, ..., 10    (inclusive end)
1..1        // empty range
5..=5       // single element: 5
```

`range` is a type that implements `stream[int]`:

```promise
type range is stream[int] {
  int start;
  int end;
  bool inclusive;

  iter() iter[int] { ... }

  // Derived ranges
  step(int n) stream[int] { ... }

  // O(1) membership test — overrides the O(n) default from Stream
  contains(int value) bool {
    if this.inclusive {
      return value >= this.start && value <= this.end;
    }
    return value >= this.start && value < this.end;
  }
}
```

Ranges compose naturally with stream combinators:

```promise
// Even numbers from 0 to 98
evens := (0..100).filter((n) -> n % 2 == 0);

// Every third number from 0 to 99
thirds := (0..100).step(3);

// Sum of 1 to 100
total := (1..=100).fold(0, (acc, n) -> acc + n);
```

### 12.4 Generator Functions

A function whose return type is `stream[T]` and whose body contains `yield` is a **generator function**. The compiler transforms its body into a state machine that implements `iter[T]`. No special modifier keyword is needed — the return type plus the presence of `yield` is sufficient.

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

// Consuming:
for n in fibonacci() {
  if n > 100 { break; }
  io.println("{n}");
}
```

**`yield*` delegates to another stream**, yielding all of its elements inline:

```promise
oneThenTwo() stream[int] {
  yield* 1..=3;     // yields 1, 2, 3
  yield* 7..=9;     // yields 7, 8, 9
}
```

**Generators with I/O** work transparently — no function coloring needed:

```promise
fetchPages(string url) stream[Page] {
  string? nextUrl = url;
  while nextUrl {
    Page page = http.get(nextUrl) ? { break; };  // stop stream on error
    yield page;
    nextUrl = page.nextLink;
  }
}
```

**Restrictions:** `yield` must appear directly in the generator function body — it cannot appear inside a nested closure or lambda. Use a `for` loop instead:

```promise
// ERROR — yield inside a closure
example() stream[int] {
  items.forEach(|item| { yield item; });  // compile error
}

// OK — yield in a for loop
example() stream[int] {
  for item in items { yield item; }
}
```

The compiler transforms each `yield` point into a state in a state machine. Local variables are captured as fields of the generated type. The `next()` method resumes execution from the last yield point and runs until the next `yield` or until the function body completes (returning `none`). This transformation is purely a compiler concern — generators look and feel like ordinary functions to the caller.

### 12.5 Collections as Streams

Built-in collection types implement `stream[T]`, giving them all stream combinators for free:

| Collection      | Stream Implementation           |
|----------------|---------------------------------|
| `T[]` (slice)   | `stream[T]` — iterates elements in order |
| `T[N]` (array)  | `stream[T]` — iterates elements in order |
| `set[T]`        | `stream[T]` — iteration order is implementation-defined |
| `map[K, V]`     | `stream[(K, V)]` — iterates key-value pairs |
| `channel[T]`    | `stream[T]` — receives from channel until closed |
| `range`         | `stream[int]` — see Section 12.3 |
| `string`        | `stream[char]` — iterates Unicode scalar values |

`map[K, V]` also provides `.keys() stream[K]` and `.values() stream[V]` for iterating only keys or values.

### 12.6 Channels as Streams

Because `channel[T]` implements `stream[T]`, channels receive all stream combinators:

```promise
ch := channel[int].new(capacity: 10);
go {
  for i in 0..100 { ch.send(i); }
  ch.close();
};

// Use stream combinators on channel
evens := ch.filter((n) -> n % 2 == 0);
for n in evens {
  io.println("{n}");
}
```

**Important difference from other streams:** channel iteration is **destructive**. Values are consumed from the shared buffer — there is no way to re-iterate a channel. Calling `iter()` on a channel returns an iterator that receives from the channel; `next()` blocks (suspends the goroutine) until a value is available or the channel is closed. When the channel is closed and empty, `next()` returns `none`.

Calling `ch.iter()` multiple times does **not** produce independent cursors over the same data — each iterator draws from the same underlying channel buffer. This is the expected behavior for a concurrent data-passing primitive.

---

## 13. Collections

Built-in collection types with generic support:

```promise
// Array (fixed-size, stack-allocated)
int[3] arr = [1, 2, 3];

// Slice (dynamic, heap-allocated)
int[] list = [1, 2, 3, 4, 5];
list.push(6);

// Map
map[string, int] scores = {
  "alice": 100,
  "bob": 85,
};

// Tuple
(int, string) pair = (42, "answer");
(num, label) := pair;      // destructuring
```

All collection types implement `stream[T]` (see Section 12.5), providing lazy combinators like `map`, `filter`, `fold`, and more:

```promise
int[] numbers = [1, 2, 3, 4, 5];
squares := numbers.map((n) -> n * n).collect();   // [1, 4, 9, 16, 25]
sum := numbers.fold(0, (acc, n) -> acc + n);       // 15
```

---

## 14. Nullable Types

Promise does not have null. Optional values use `Option[T]`, with sugar `T?`:

```promise
find(int id) User? {            // shorthand for Option[User]
  // ...
  return none;                   // Option.None
}
```

A value of type `T` is **implicitly convertible** to `T?`. No wrapping syntax is needed:

```promise
string name = "Alice";
string? maybeName = name;       // OK — implicit T → T?
```

### 14.1 Working with Optionals

There are three ways to test and unwrap optional values, from lightest to most explicit.

#### Truthiness narrowing

When a `T?` value appears as an `if` condition, it is treated as a presence check. Inside the block, the compiler **narrows** the type from `T?` to `T`:

```promise
string? cc = getCC();

if cc {
  io.println(cc);              // cc is string here, not string?
}

if !cc {
  io.println("no value");
  io.println(cc);               // ERROR: cc is known to be none here
}
```

Inside the `if cc` block, `cc` is narrowed to `T`. Inside the `if !cc` block, `cc` is known to be `none` — any use of `cc` as type `T` is a **compile-time error**.

This works for any `T?` where `T` is not `bool`. For `bool?`, the compiler emits an error because the intent is ambiguous — use `is present` instead (see below).

#### `is present` / `is absent`

For explicit presence testing that works with **any** `T?` — including `bool?` — use the `is present` and `is absent` patterns. These are contextual keywords: `present` and `absent` are only special after `is` in pattern position; in all other contexts they are normal identifiers.

```promise
bool? verbose = getFlag();

if verbose is present {
  // verbose is bool here — narrowed from bool?
  if verbose { enableLogging(); }
} else {
  io.println(verbose);           // ERROR: verbose is known to be none here
}

if verbose is absent {
  io.println("no flag provided");
  io.println(verbose);           // ERROR: verbose is known to be none here
}
```

As with truthiness narrowing, the inverse blocks enforce negative narrowing: inside `is absent` or the `else` of `is present`, the variable is known to be `none` and any use of it as type `T` is a **compile-time error**.

`is present` and `is absent` extend the existing `is` pattern matching keyword (see Section 10.3 for the full `is` keyword disambiguation table). They cannot collide with type names — `present` and `absent` are contextual keywords recognized only after `is` in pattern position.

#### Unwrap binding with `:=`

When you want to unwrap into a **new name**, use `:=` inside an `if` condition:

```promise
User? user = find(42);

if u := user {
  io.println(u.name);          // u is User — unwrapped
}
```

If the right side is `none`, the condition is false. If it holds a value, the left side is bound to the unwrapped value and the block executes. This is useful when you want a shorter or more descriptive name than the original variable.

The `:=` unwrap binding also works in `while` conditions, looping as long as the expression produces a value:

```promise
while item := iter.next() {
  process(item);              // item is T, not T?
}
```

This is the mechanism underlying `for-in` loop desugaring (see Section 12.2).

### 14.2 Other Optional Operations

```promise
// `?.` chaining — short-circuits to none if the receiver is absent
name := find(42)?.name;

// `?:` default operator — returns the right side if the left is none
name := find(42)?.name ?: "unknown";

// match for complex enum patterns
match result {
  Ok(value) => process(value),
  Err(e) => log(e),
}
```

### 14.3 Optional Parameters

When `T?` is used as a **function/method parameter type**, the parameter is implicitly optional — the caller may omit it, and the function receives `none` (see Section 9.3). To declare a required parameter of type `Option[T]`, use `Option[T]` explicitly instead of the `T?` sugar. For how `T?` interacts with stream iteration, see Section 12.

---

## 15. Unsafe Code

Promise allows unsafe blocks for low-level operations:

```promise
rawPointer() `unsafe {
  ptr := unsafe {
    int* raw = alloc[int]();
    *raw = 42;
    raw
  };
}
```

---

## 16. Resource Management

Promise provides deterministic, ownership-driven resource cleanup without garbage collector finalizers. Two mechanisms work together: **`use` bindings** for scoped resource lifetime, and **`drop()` methods** for general cleanup when an owner goes out of scope.

### 16.1 I/O Interfaces

The standard library defines a set of structural interfaces for I/O, following the Go model of small, composable abstractions:

```promise
type Reader `structural {
  read(~this, u8[] ~buf) int! `abstract `instance;
}

type Writer `structural {
  write(~this, u8[] &buf) int! `abstract `instance;
}

type Closer `structural {
  close(~this)! `abstract `instance;
}

type Seeker `structural {
  seek(~this, int offset, int whence) int! `abstract `instance;
}
```

Compound interfaces compose these via inheritance:

```promise
type ReadCloser `structural is Reader, Closer {}
type WriteCloser `structural is Writer, Closer {}
type ReadWriter `structural is Reader, Writer {}
type ReadWriteCloser `structural is Reader, Writer, Closer {}
```

Because these are `` `structural ``, any type with matching method signatures satisfies them automatically — no explicit `is` declaration needed. A `File` type satisfies `ReadWriteCloser` by having `read`, `write`, and `close` methods with the right signatures:

```promise
type File is ReadWriteCloser {
  int fd;

  open(string path, string mode) File! `type `native;

  read(~this, u8[] ~buf) int! `instance `native;
  write(~this, u8[] &buf) int! `instance `native;
  close(~this)! `instance `native;
  seek(~this, int offset, int whence) int! `instance `native;
}
```

User-defined types participate naturally:

```promise
type BufferedWriter is Writer {
  Writer inner;
  u8[] buf;

  write(~this, u8[] &data) int! `instance {
    this.buf.push(data);
    if this.buf.len >= 4096 {
      return this.flush()?;
    }
    return data.len;
  }

  flush(~this) int! `instance {
    n := this.inner.write(&this.buf)?;
    this.buf = [];
    return n;
  }
}
```

### 16.2 `use` Bindings

A `use` binding ties a resource's lifetime to the enclosing scope. When the scope exits — whether by normal fall-through, `return`, `raise`, `break`, or `continue` — the compiler automatically calls `close()` on the bound variable.

```promise
main()! {
  use f := File.open("data.txt", "r")?;
  string data = f.readAll()?;
  // f.close() called automatically here
}
```

`use` works in any block scope:

```promise
process(string path)! {
  use f := File.open(path, "r")?;

  if needsBackup(path) {
    use backup := File.open(path + ".bak", "w")?;
    copyTo(f, backup)?;
    // backup.close() called here
  }

  // f.close() called here
}
```

**Rules:**

- The variable's type must have a `close()` method (checked via structural or nominal satisfaction of `Closer`)
- Multiple `use` bindings in the same scope are closed in **reverse declaration order** (LIFO), matching the intuition that resources opened later depend on resources opened earlier
- A `use` variable cannot be moved out of its scope — the compiler must guarantee `close()` is callable
- `use` is a variable declaration modifier, not a statement — it binds a name and a value

**Close error handling:** If the enclosing function is failable (`!` return type), close errors propagate — but only if no other error is already in flight. If the scope is exiting due to a `raise` or error propagation, the close error is suppressed to preserve the original error. If the scope exits normally and `close()` fails, the close error becomes the function's error.

```promise
// In a failable function:
writeData(string path, u8[] &data)! {
  use f := File.open(path, "w")?;
  f.write(&data)?;
  // If f.close() fails here, the error propagates (no prior error)
}

// If write fails:
writeData(string path, u8[] &data)! {
  use f := File.open(path, "w")?;
  f.write(&data)?;  // raises an error
  // f.close() still called, but its error is suppressed — write's error propagates
}
```

In a non-failable function, close errors are silently suppressed (there is nowhere to propagate them).

### 16.3 The `drop()` Method

Any type can define a `drop()` method for general-purpose cleanup. The compiler inserts `drop()` calls when a value's owner goes out of scope and the value has not been moved.

```promise
type TempFile {
  string path;
  File file;

  drop(~this) `instance {
    this.file.close();
    fs.remove(this.path);
  }
}
```

**Drop rules:**

1. When a variable goes out of scope and its type has a `drop()` method, the compiler inserts a `drop()` call
2. If the value was **moved** (ownership transferred), no drop occurs — the new owner is responsible
3. Fields are dropped recursively **after** the parent's `drop()` runs, in reverse declaration order
4. `` `copy `` types cannot define `drop()` — copy semantics are incompatible with unique cleanup (a value that is bitwise-copied would have its `drop()` called multiple times)
5. Drop order within a scope follows reverse declaration order (LIFO), consistent with `use` bindings
6. `drop()` methods must not fail — the signature is `drop(~this)` with no `!` return

**Conditional drops:** When ownership depends on control flow, the compiler inserts **drop flags** — boolean variables that track whether a value still needs dropping:

```promise
transfer(bool condition) {
  Resource r = Resource.new();
  if condition {
    consume(r);  // r is moved — no drop needed
  }
  // Compiler: if !moved_r { r.drop(); }
}
```

### 16.4 Interaction Between `use` and `drop()`

`use` and `drop()` are complementary:

- `use` calls `close()` at scope exit — for explicit, scoped resource management
- `drop()` calls cleanup code at scope exit — for automatic, ownership-driven cleanup
- A type can have both: `use` calls `close()` explicitly, and `drop()` handles cleanup for non-`use` variables

If a variable is declared with `use`, the compiler calls `close()` at scope exit. If that same type also has a `drop()` method, `drop()` is **not** called — `use` takes precedence. The expectation is that `close()` performs all necessary cleanup (or that `drop()` delegates to `close()` internally).

For variables **not** declared with `use`, normal `drop()` semantics apply — the compiler calls `drop()` when the owner goes out of scope.

```promise
type Connection {
  int socket_fd;

  close(~this)! `instance {
    syscall.close(this.socket_fd)?;
  }

  drop(~this) `instance {
    // Best-effort close — errors suppressed in drop
    syscall.close(this.socket_fd);
  }
}

main()! {
  // Explicit scoped lifetime — close() called, errors can propagate
  use conn := Connection.connect("localhost:5432")?;
  conn.query("SELECT 1")?;
  // conn.close() called here — error propagates if function is !

  // Without use — drop() called at scope exit, errors suppressed
  conn2 := Connection.connect("localhost:5432")?;
  conn2.query("SELECT 1")?;
  // conn2.drop() called here — best-effort cleanup
}
```

### 16.5 Summary

| Mechanism | Trigger | Method Called | Error Handling | Use Case |
|-----------|---------|-------------|----------------|----------|
| `use` binding | Scope exit | `close()` | Propagates in `!` functions (unless another error is in flight) | Explicit resource scoping |
| `drop()` method | Owner out of scope (not moved) | `drop()` | Suppressed (drop must not fail) | Automatic cleanup |
| Neither | Owner out of scope | Nothing | N/A | `` `copy `` types, primitives |

---

## 17. Concurrency

Promise uses goroutine-style lightweight coroutines. The runtime multiplexes goroutines onto OS threads and transparently handles I/O scheduling — all blocking I/O calls automatically suspend the current goroutine and resume it when the operation completes. There is **no function coloring**: functions that perform I/O have normal signatures and look identical to pure functions.

### 17.1 Transparent I/O

Functions are never declared as "async". The runtime is the async engine — any function that performs I/O automatically yields the goroutine during the blocking operation:

```promise
// This function does I/O but has a normal signature.
// The runtime suspends the goroutine during httpGet, not the OS thread.
fetchUser(int id) User! {
  data := httpGet("/users/{id}")?;
  return User.fromJson(data)?;
}

// Callers just call it normally:
user := fetchUser(42)?;
```

### 17.2 Explicit Concurrency with `go`

`go` is an **expression** that launches a goroutine and returns a `task[T]`, where `T` is the result type of the block or call. The `<-` operator receives the result, suspending the current goroutine until it is ready.

```promise
// Fire-and-forget (task[Void] result ignored)
go {
  logAnalytics(event);
};

// Value-returning task
task := go fetchUser(42);          // task : task[User!]
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

`task[T]` is a first-class type returned by `go` expressions. It can be stored in variables, fields, and collections, passed as arguments, and returned from functions. The `<-` operator receives the result from a task, suspending the current goroutine until the task completes. Concurrency is always a **caller-side decision** — the callee does not know or care whether it runs in a goroutine.

### 17.3 Channels

Channels are the primary synchronization primitive for streaming data between goroutines:

```promise
main() {
  ch := channel[int].new(capacity: 10);

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

Because `channel[T]` implements `stream[T]`, channels gain all stream combinators — `map`, `filter`, `fold`, etc. — for free. See Section 12.6 for details and caveats about destructive iteration.

### 17.4 Ownership Across Goroutines

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

## 18. Complete Example

```promise
use io "github.com/promise-lang/std/io/1"
use json "github.com/promise-lang/std/json/1"

type Todo `serializable {
  int id `json(name: "id");
  string title `json(name: "title");
  bool done = false;                     // field default — constructor can skip

  toggle(~this) `instance {
    this.done = !this.done;
  }

  new(int id, string title) Todo `type {
    return Todo(id: id, title: title);   // done defaults to false
  }
}

type TodoList {
  Todo[] items;

  add(~this, string title, int priority = 0) `instance {
    int id = this.items.len() + 1;
    this.items.push(Todo.new(id, title));
  }

  pending(&this) Todo[] `instance {
    return this.items.filter(|t| !t.done).collect();
  }
}

loadFromFile(string &path) TodoList! {
  string content = io.readFile(path);          // auto-propagates on error
  Todo[] items = json.decode[Todo[]](content); // auto-propagates on error
  return TodoList(items: items);
}

main() {
  TodoList todos = loadFromFile("todos.json") ? err {
    io.println("Starting fresh: {err.message()}");
    TodoList(items: []);
  };

  todos.add("Design Promise language");
  todos.add("Build the compiler", priority: 1);   // named arg

  for i, todo in todos.items {
    status := if todo.done { "done" } else { "    " };
    io.println("[{status}] {todo.title}");
  }
}
```

---

## 19. Grammar Sketch (ANTLR4)

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
fieldDecl: typeRef IDENT metaAnnotation* ('=' expression)? ';';
methodDecl: IDENT '(' params ')' returnType? metaAnnotation* (block | ';');

funcDecl: IDENT typeParams? '(' params ')' returnType? metaAnnotation* block;
returnType: typeRef '!'?;

// Parameters (definition side)
params: paramList?;
paramList: receiverParam (',' param)* | param (',' param)*;
receiverParam: refMod? 'this';
param: typeRef refMod? IDENT ('=' expression)?;
refMod: '&' | '~';

// Arguments (call site)
args: (arg (',' arg)*)?;
arg: (IDENT ':')? expression;             // named (IDENT ':') or positional

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
    | typeRef '?'                        // optional (postfix)
    | typeRef '[' ']'                    // slice
    | typeRef '[' INT_LITERAL ']'        // fixed array
    | IDENT typeArgs?                    // named type
    | '(' typeRef (',' typeRef)* ')'     // tuple
    | '(' typeRefList ')' '->' typeRef   // function type (arrow syntax)
    ;

typeArgs: '[' typeRef (',' typeRef)* ']';
typeRefList: typeRef (',' typeRef)*;

// Core expression and statement productions (simplified)
block: '{' statement* '}';
statement: expression ';' | varDecl | useDecl | assignment | returnStmt | raiseStmt
         | ifStmt | forStmt | whileStmt | yieldStmt | yieldDelegateStmt;

useDecl: 'use' IDENT ':=' expression ';';     // scoped resource — close() at scope exit

expression: primary | expression binOp expression | unaryOp expression
          | expression '.' IDENT | expression '(' args ')'
          | expression '?' IDENT? block             // error handler
          | expression '?' | expression '!'         // propagate / unwrap
          | expression 'as' '!'? typeRef            // type cast: safe (as) or unsafe (as!)
          | goExpr | receiveExpr | rangeExpr | isExpr
          | ifExpr | matchExpr | '(' expression ')';

// Control flow
ifStmt: 'if' expression block ('else' (ifStmt | block))?;
ifExpr: 'if' expression block 'else' block;
whileStmt: 'while' expression block;
forInStmt: 'for' IDENT (',' IDENT)? 'in' expression block;
classicForStmt: 'for' varDecl ';' expression ';' expression block;
forStmt: forInStmt | classicForStmt | 'for' block;   // infinite loop

goExpr: 'go' (block | expression);    // returns task[T]
receiveExpr: '<-' expression;          // receive from task[T] or channel[T]

// Error handling
errorPropagate: expression '?';                          // explicit propagate
errorHandler: expression '?' IDENT? block;               // ? e { ... } or ? { ... }
errorUnwrap: expression '!';                             // panic on error
resultDestructure: '(' IDENT ',' IDENT ')' ':=' expression;  // (val, err) := expr

// range expressions
rangeExpr: expression '..' '='? expression;    // 0..10 (exclusive) or 0..=10 (inclusive)

// Yield (only valid inside generator functions returning stream[T])
yieldStmt: 'yield' expression ';';
yieldDelegateStmt: 'yield' '*' expression ';';

// Pattern matching — `is` expression and `match` expression
isExpr: expression 'is' pattern;
matchExpr: 'match' expression '{' matchArm (',' matchArm)* ','? '}';
matchArm: matchPattern ('if' expression)? '=>' (expression | block);

matchPattern
    : '_'                                    // wildcard
    | LITERAL                                // literal (int, string, bool)
    | IDENT '.' IDENT                        // enum variant: Color.Red
    | IDENT '.' IDENT '(' patternFields ')'  // enum destructure: Color.Custom(r, g, b)
    | IDENT IDENT?                           // type pattern with optional binding: Dog d
    ;

pattern
    : IDENT '(' patternFields ')'       // enum destructuring: Some(u), Ok(v)
    | IDENT                              // type check: Dog, or contextual: present, absent
    ;
patternFields: patternField (',' patternField)*;
patternField: IDENT;

// Unwrap binding in if/while conditions
ifUnwrap: 'if' IDENT ':=' expression block ('else' block)?;
whileUnwrap: 'while' IDENT ':=' expression block;

lifetime: '\'' IDENT;
```

---

## 20. Compiler Implementation Plan (Go)

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

## 21. Package Manager (integrated into `promise` binary)

### Dependency Resolution

- Dependencies are discovered by scanning all `use` declarations in source files — no separate dependency manifest.
- Modules fetched from their URL (git clone / HTTP archive).
- Version segments in the URL are the version specifier — no separate version field.
- Lockfile (`promise.lock`) pins exact commit hashes.
- Diamond dependency resolution: if two deps require different versions of the same module, both are compiled separately (like Go's module system).

---

## 22. Open Design Questions

1. **REPL** — Should the toolchain include an interpreter/REPL for rapid prototyping?
2. **Stream backpressure** — When a generator yields into a channel-backed consumer, should there be built-in backpressure beyond channel's existing capacity mechanism?
3. **Parallel stream execution** — Should `stream[T]` have a `.parallel()` combinator that distributes work across goroutines? If so, how does ordering work?
4. **Stream error handling** — Should `stream[T]` support `stream[T!]` where individual elements can carry errors? Or should a failing generator terminate the stream entirely?
