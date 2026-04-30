# C Binding Architecture — Generated Header Approach

> **Status: Implemented but dormant.** The extern ABI coercion (`extern.go`) and header generation (`headergen.go`) were built and are tested. However, the original motivation — type-safe binding to a C runtime — is obsolete: all runtime functions were migrated to codegen-emitted LLVM IR (see [runtime-proposal.md](runtime-proposal.md), Phases 1-4). The `extern` keyword and ABI coercion remain actively used for PAL function declarations in `std/*.pr`. Header generation could be revived for future C FFI if needed.

## Problem (historical)

The C binding described below was fragile and wouldn't scale:

1. **Hardcoded mappings.** `builtins.go` manually declares `promise_print_int`, `promise_print_f64`, etc. `declareExternFunc` pattern-matches "1 arg, void return, print-like" to select a runtime function. Every new extern requires touching three places: `declareRuntime()`, `declareExternFunc()`, and `runtime.c`.

2. **No type verification.** The LLVM IR codegen emits `call void @promise_print_bool(i8 %x)` and `runtime.c` defines `void promise_print_bool(char x)`. These happen to agree, but nothing checks that. A mismatch (e.g., changing bool to `i16` in codegen but not in C) silently produces undefined behavior.

3. **No compound types.** When strings, arrays, slices, user structs, and enums cross the C boundary, both sides must agree on struct layout, field order, padding, and pointer indirection. The current approach has no mechanism for this.

4. **No type coercion framework.** `genCallExpr` special-cases `isRuntimePrintFunc` for i1→i8 coercion. Every new extern function type needs its own coercion code.

---

## Proposed Architecture

### Core idea

The compiler generates a C header (`.h`) from the extern function declarations and all types reachable from their signatures. The C runtime `#include`s this header. When `clang` compiles the runtime, it verifies that the C implementations match the declared types. The LLVM IR codegen uses the same type layout definitions that produced the header, guaranteeing both sides agree.

### New component: `TypeDeclLayout`

A **shared layout computation** runs after sema, before both codegen and header generation. It produces a `TypeDeclLayout` for every type that appears in an extern signature (transitively). Each `TypeDeclLayout` contains up to **four struct layouts** — one for each level of the four-struct model.

```
Source (.pr) → Parse → Sema → Ownership
                                  ↓
                           Layout computation  ←── single source of truth
                            ↙           ↘
                   C header gen       LLVM IR codegen
                      ↓                    ↓
                 promise.h            program.ll
                      ↓
              clang -c runtime.c -include promise.h → runtime.o
                                           ↓
                              clang program.ll runtime.o → binary
```

Both the header generator and the IR codegen consume the same layout map, so they cannot disagree.

---

## Type Layout and the Four-Struct Model

Every Promise type declaration produces **four LLVM structs**: Value (`T#v`), Instance (`T#i`), Variant (`T#m`), and Type (`T#t`). The C binding must respect this — each type declaration gets four layouts, not one.

### Data structure

```go
// TypeDeclLayout holds all four struct layouts for a Promise type declaration.
// For primitives, only Value is populated (Instance/Variant/Type are nil).
// For user types, all four are populated based on field placement annotations.
type TypeDeclLayout struct {
    PromiseName string              // Promise type name: "Point", "int"
    Kind        LayoutKind          // primitive, struct, enum, array, etc.
    Value       *StructLayout       // T#v — vtable_ptr + instance_ptr + `value fields
    Instance    *StructLayout       // T#i — variant_ptr + default fields
    Variant     *StructLayout       // T#m — type_ptr + `variant fields
    Type        *StructLayout       // T#t — `type fields + metadata
}

// StructLayout describes one of the four C-ABI-compatible struct representations.
type StructLayout struct {
    CName        string            // C identifier: "promise_Point_v", "promise_Point_i"
    Suffix       string            // "_v", "_i", "_m", "_t"
    Size         int64             // size of FIXED part in bytes (excludes flexible array)
    Align        int64             // alignment in bytes
    Fields       []FieldLayout     // ordered fields including internal pointers
    LLVMType     irtypes.Type      // resolved LLVM struct type
    IsFlexible   bool              // true if last field is a C99 flexible array member
    FlexElemType *TypeDeclLayout   // element type of the flexible member (nil if !IsFlexible)
}

type FieldLayout struct {
    Name       string          // field name (e.g., "x", "_vtable", "_instance")
    CName      string          // C field name
    CType      string          // C type string for header generation
    Offset     int64           // byte offset within the struct
    Layout     *TypeDeclLayout // recursive layout (nil for primitives/pointers)
    IsInternal bool            // true for _vtable, _instance, _variant, _type pointers
}

type LayoutKind int
const (
    LayoutPrimitive  LayoutKind = iota  // int, f64, bool, char — single LLVM scalar
    LayoutStruct                         // Named type with fields
    LayoutEnum                           // tagged union
    LayoutArray                          // fixed-size T[N]
    LayoutSlice                          // dynamic T[]
    LayoutTuple                          // (T1, T2, ...)
    LayoutOptional                       // T? — tag + value
    LayoutString                         // string — special handling
    LayoutPointer                        // T*, T&, T~
)
```

### How fields map to structs

When computing layouts, fields are distributed based on their `Placement` annotation:

```promise
type Player {
    String name;              // PlaceInstance (default) → Instance struct
    Int health;               // PlaceInstance (default) → Instance struct
    Float64 x `value;         // PlaceValue              → Value struct
    Float64 y `value;         // PlaceValue              → Value struct
    String sprite `variant;   // PlaceVariant             → Variant struct
    Int maxPlayers `type;     // PlaceType                → Type struct
}
```

Produces four layouts:

```go
// Player Value struct (T#v)
Value: {
    CName: "promise_Player_v",
    Fields: [
        { Name: "_vtable",   CType: "void*",  IsInternal: true },
        { Name: "_instance", CType: "void*",  IsInternal: true },
        { Name: "x",         CType: "double", IsInternal: false },
        { Name: "y",         CType: "double", IsInternal: false },
    ],
}

// Player Instance struct (T#i)
Instance: {
    CName: "promise_Player_i",
    Fields: [
        { Name: "_variant",  CType: "void*",          IsInternal: true },
        { Name: "name",      CType: "promise_string_v", IsInternal: false },
        { Name: "health",    CType: "int64_t",           IsInternal: false },
    ],
}

// Player Variant struct (T#m)
Variant: {
    CName: "promise_Player_m",
    Fields: [
        { Name: "_type",   CType: "void*",            IsInternal: true },
        { Name: "sprite",  CType: "promise_string_v",  IsInternal: false },
    ],
}

// Player Type struct (T#t)
Type: {
    CName: "promise_Player_t",
    Fields: [
        { Name: "maxPlayers", CType: "int64_t", IsInternal: false },
        // + compiler-generated metadata fields
    ],
}
```

### Uniform passing convention: always `promise_T_v*` (pointer)

Since the Value struct always contains `_instance` (a pointer to `T#i`), the extern boundary uses a **single rule with no special cases**:

- **Always pass `promise_T_v*`** — for all parameters (by-value and references alike)
- **Struct returns use sret** — the function returns `void`, with the first parameter being a pointer to the result storage

All extern params are passed **by pointer** (not by value). This ensures correct behavior on ARM64 where the C ABI passes structs >16 bytes indirectly. The LLVM IR declares all extern value params as `i8*` and generates alloca + store + bitcast-to-pointer at each call site. The generated C header declares matching `promise_T_v *param` signatures.

C code that needs instance-level data follows `val->_instance->field`. No need for separate calling conventions per struct level.

This applies to **all types including primitives**. For `int`, the value struct is:

```c
typedef struct {
    void*          _vtable;
    promise_int_i* _instance;   // null for primitives (no instance data)
    int64_t        raw;
} promise_int_v;
```

The `_instance` pointer is null for primitives because all their data lives in the value struct (the `raw` field). C code has no reason to dereference it. **No special case needed** — the same layout computation and header generation applies to every type uniformly.

**Consequence:** the `_instance` field is **typed** (not `void*`), so the header must declare `T#i` before `T#v` for any type that has instance fields. For primitives, `_instance` is typed as `void*` (or the empty instance struct pointer) since the instance struct has no user-visible fields.

| Usage in extern signature | LLVM IR param | C header param | Rationale |
|---|---|---|---|
| `T` by value | `i8*` | `promise_T_v *x` | Pointer to value struct (ARM64 ABI compatible) |
| `T&` shared reference | `%promise_T_v*` | `promise_T_v *x` | Typed pointer to value struct |
| `T~` mutable reference | `%promise_T_v*` | `promise_T_v *x` | Typed pointer to value struct |
| Return `T` | sret: `i8*` first param | `promise_T_v *sret` first param | Struct return via pointer (sret pattern) |
| Return void | `void` | `void` | No sret needed |

Since all four structs are linked (`_v` → `_i` → `_m` → `_t`), the header always emits the full chain for any reachable type. This is just struct typedefs — cheap to emit, and it ensures C code can navigate the full object graph when needed.

### Recursive struct embedding

When a field in one of the four structs is itself a compound type, the layout must embed the appropriate struct level of the field type:

- A `value` field of type `String` embeds `promise_string_v` (the String Value struct)
- An `instance` field of type `String` embeds `promise_string_v` as well (values are the unit of storage — instance fields store values)

The general rule: **field storage always uses the Value struct representation of the field's type.** References (`&T`, `~T`) store pointers to the Instance struct.

### Primitive type layouts

Primitives follow the same four-struct model as every other type. Their `TypeDeclLayout` has all four levels, but Instance/Variant/Type structs have no user fields — only the internal pointer. All data lives in the Value struct via the `` `raw `value `` field.

```go
// int → TypeDeclLayout{
//   PromiseName: "int",
//   Kind: LayoutPrimitive,
//   Value:    { CName: "promise_int_v",
//               Fields: [_vtable(void*), _instance(promise_int_i*), raw(int64_t)] }
//   Instance: { CName: "promise_int_i",
//               Fields: [_variant(promise_int_m*)] }  // no user fields
//   Variant:  { CName: "promise_int_m",
//               Fields: [_type(promise_int_t*)] }      // no user fields
//   Type:     { CName: "promise_int_t", Fields: [] }   // metadata only
// }
```

At the C ABI level, primitives are passed as `promise_T_v` structs like everything else. The `_instance` pointer is null at runtime (no instance data exists), but the struct layout is the same.

| Promise type | Raw field C type | Raw field LLVM type | Value struct fields |
|---|---|---|---|
| `int`, `i64` | `int64_t` | `i64` | `{ void*, void*, int64_t }` |
| `i32` | `int32_t` | `i32` | `{ void*, void*, int32_t }` |
| `i16` | `int16_t` | `i16` | `{ void*, void*, int16_t }` |
| `i8` | `int8_t` | `i8` | `{ void*, void*, int8_t }` |
| `uint`, `u64` | `uint64_t` | `i64` | `{ void*, void*, uint64_t }` |
| `u32` | `uint32_t` | `i32` | `{ void*, void*, uint32_t }` |
| `u16` | `uint16_t` | `i16` | `{ void*, void*, uint16_t }` |
| `u8` | `uint8_t` | `i8` | `{ void*, void*, uint8_t }` |
| `f64` | `double` | `double` | `{ void*, void*, double }` |
| `f32` | `float` | `float` | `{ void*, void*, float }` |
| `bool` | `uint8_t` | `i8` | `{ void*, void*, uint8_t }` |
| `char` | `int32_t` | `i32` | `{ void*, void*, int32_t }` |

**Note on `bool`:** Internally Promise uses `i1` for booleans, but the value struct stores it as `i8` (the `` `raw `` field type). The codegen inserts `zext i1 → i8` when storing into the value struct and `trunc i8 → i1` when loading. This is consistent with all types — the value struct always uses the declared raw type.

### Compound type layouts

#### Named types — four-struct C header output

For `Player` above, the generated header emits all four structs in dependency order (`_t` → `_m` → `_i` → `_v`):

```c
// Player Type struct (T#t) — per-declaration metadata
typedef struct {
    int64_t maxPlayers;
} promise_Player_t;

// Player Variant struct (T#m) — per-monomorphization
typedef struct {
    promise_Player_t*  _type;
    promise_string_v   sprite;
} promise_Player_m;

// Player Instance struct (T#i) — heap-allocated object
typedef struct {
    promise_Player_m*  _variant;
    promise_string_v   name;
    int64_t            health;
} promise_Player_i;

// Player Value struct (T#v) — the unit of passing
typedef struct {
    void*              _vtable;
    promise_Player_i*  _instance;   // typed! C code can do val._instance->name
    double             x;
    double             y;
} promise_Player_v;
```

**Internal pointers are typed**, not `void*` (except `_vtable` which points to a generated vtable struct that varies per view). This gives C code natural navigation: `player._instance->name`, `player._instance->_variant->sprite`, etc. The leading `_` convention signals these are managed by the runtime — C code should read but not write them.

#### Value-only types (pure value semantics)

When all fields are `value` fields and no instance/variant/type user fields exist:

```promise
type Point {
    f64 x `value;
    f64 y `value;
}
```

All four structs are still emitted — instance/variant/type are minimal (internal pointer only):

```c
typedef struct { } promise_Point_t;
typedef struct { promise_Point_t* _type; } promise_Point_m;
typedef struct { promise_Point_m* _variant; } promise_Point_i;
typedef struct {
    void*             _vtable;
    promise_Point_i*  _instance;   // null at runtime (no instance data)
    double            x;
    double            y;
} promise_Point_v;
```

The `_instance` is null at runtime because there are no instance fields. C code works entirely with the value struct fields (`x`, `y`). No special case — the same layout computation applies.

#### Flexible array members (inline data)

Strings and containers use **C99 flexible array members** to store variable-length data inline in the Instance struct, eliminating a separate heap allocation for the data buffer. The last field of the Instance struct is declared with `[]` syntax (zero-length in C, `[0 x T]` in LLVM IR) and the allocation is sized to accommodate the actual data:

```c
// C99 flexible array member
typedef struct {
    int64_t len;
    char    data[];   // flexible — sizeof(struct) excludes this
} my_string;

// Allocation: malloc(sizeof(my_string) + len)
```

In LLVM IR, the corresponding type uses a zero-length array:

```llvm
%my_string = type { i64, [0 x i8] }
; Allocation: call ptr @malloc(i64 %total)  where total = sizeof_fixed + len
; Access:     getelementptr %my_string, ptr %s, i32 0, i32 1, i64 %idx
```

This pattern applies to strings (inline character data) and arrays/slices (inline elements). The `StructLayout` tracks this via `IsFlexible` and `FlexElemType`.

**Implications:**
- Instance structs with flexible array members cannot be copied with struct assignment — use `memcpy` with the correct total size.
- `sizeof()` in C returns only the fixed part. The true allocated size is `sizeof(fixed) + count * sizeof(element)`.
- These types are **immutable in-place** — appending or resizing requires allocating a new instance. This is desirable for strings (immutable by design) and acceptable for growable arrays (amortized reallocation).

#### Strings

`string` stores its length and character data in the Instance struct, with the data inline via a flexible array member. The Value struct is a lightweight handle (just `_vtable` + `_instance`):

```promise
type string `intrinsic {
    int len;    // PlaceInstance (default) → Instance struct
    // char data[] is compiler-managed inline storage
}
```

```c
typedef struct { } promise_string_t;
typedef struct { promise_string_t* _type; } promise_string_m;
typedef struct {
    promise_string_m*  _variant;
    int64_t            len;
    char               data[];    // C99 flexible array member — UTF-8 encoded
} promise_string_i;
typedef struct {
    void*              _vtable;
    promise_string_i*  _instance;
} promise_string_v;
```

LLVM: Value is `{ i8*, %promise_string_i* }`, Instance is `{ %promise_string_m*, i64, [0 x i8] }`

The Instance struct is allocated with `sizeof(promise_string_i) + len` bytes — exactly enough for the data, **no trailing null terminator**. Promise strings may contain embedded `\0` characters, so null termination is meaningless and mixing length-based and null-terminated conventions is a dangerous source of bugs. C runtime code must always use `len` to determine string boundaries. Strings are **immutable** — modifying a string requires allocating a new instance.

C code accesses string data naturally (all extern params are passed by pointer):

```c
void promise_print_string(promise_string_v *s) {
    // s->_instance->len  — string length
    // s->_instance->data — UTF-8 character data (NOT null-terminated, may contain \0)
    fwrite(s->_instance->data, 1, s->_instance->len, stdout);
}
```

#### Fixed arrays (`T[N]`) and slices (`T[]`)

These are generic instances. Each produces four structs. Elements are stored **inline** in the Instance struct via a flexible array member, following the same pattern as strings:

```c
// array[int] — elements inline in instance
typedef struct { } promise_array_int_t;
typedef struct { promise_array_int_t* _type; } promise_array_int_m;
typedef struct {
    promise_array_int_m* _variant;
    int64_t              len;
    int64_t              cap;       // allocated capacity (for growable arrays)
    int64_t              data[];    // inline elements (C99 flexible array member)
} promise_array_int_i;
typedef struct {
    void*                _vtable;
    promise_array_int_i* _instance;
} promise_array_int_v;
```

For slices (`int[]`):
```c
typedef struct { } promise_slice_int_t;
typedef struct { promise_slice_int_t* _type; } promise_slice_int_m;
typedef struct {
    promise_slice_int_m* _variant;
    int64_t              len;
    int64_t              cap;
    int64_t              data[];    // inline elements
} promise_slice_int_i;
typedef struct {
    void*                _vtable;
    promise_slice_int_i* _instance;
} promise_slice_int_v;
```

LLVM: Instance is `{ %promise_array_int_m*, i64, i64, [0 x i64] }`, Value is `{ i8*, %promise_array_int_i* }`

The Instance struct is allocated with `sizeof(promise_array_int_i) + cap * sizeof(int64_t)` bytes. Growing a mutable array requires `realloc` of the entire instance, then updating the value's `_instance` pointer. With unique ownership, only one pointer needs updating; shared references require the ownership system to track aliasing.

C code accesses elements naturally via `arr._instance->data[i]`.

#### Enums (tagged unions)

Enums also follow the four-struct model, but with a tag field:

```promise
enum Shape {
    Circle(f64 radius)
    Rect(f64 w, f64 h)
}
```

The Value struct carries the tag and enough space for the largest variant's value fields:

```c
typedef struct { } promise_Shape_t;
typedef struct { promise_Shape_t* _type; } promise_Shape_m;
typedef struct { promise_Shape_m* _variant; } promise_Shape_i;
typedef struct {
    void*             _vtable;
    promise_Shape_i*  _instance;   // null (enum data is in value struct)
    int32_t           tag;         // 0 = Circle, 1 = Rect
    union {
        struct { double radius; } circle;
        struct { double w; double h; } rect;
    };
} promise_Shape_v;
```

LLVM: `{ i8*, %promise_Shape_i*, i32, [max_variant_size x i8] }` with bitcasts per variant.

If enum variants have instance-level fields, those go in the Instance struct with the tag repeated for safety.

#### Optional (`T?`)

```c
typedef struct { } promise_optional_int_t;
typedef struct { promise_optional_int_t* _type; } promise_optional_int_m;
typedef struct { promise_optional_int_m* _variant; } promise_optional_int_i;
typedef struct {
    void*                    _vtable;
    promise_optional_int_i*  _instance;
    uint8_t                  has_value;   // 0 = none, 1 = some
    int64_t                  value;       // only valid when has_value == 1
} promise_optional_int_v;
```

#### Tuples

```c
typedef struct { } promise_tuple_int_f64_t;
typedef struct { promise_tuple_int_f64_t* _type; } promise_tuple_int_f64_m;
typedef struct { promise_tuple_int_f64_m* _variant; } promise_tuple_int_f64_i;
typedef struct {
    void*                      _vtable;
    promise_tuple_int_f64_i*   _instance;
    int64_t                    _0;
    double                     _1;
} promise_tuple_int_f64_v;
```

#### Generic instances

Generic instantiations are monomorphized. `array[int]` and `array[f64]` produce separate `TypeDeclLayout`s, each with four structs:

```c
typedef struct { ... } promise_array_int_v;
typedef struct { ... } promise_array_int_i;
typedef struct { ... } promise_array_f64_v;
typedef struct { ... } promise_array_f64_i;
```

The mangling scheme: `promise_TypeName_Arg1_Arg2_{suffix}`. Nested generics: `promise_map_string_array_int_v`.

### Future optimization: eliding internal pointers

For primitives and `copy` types where the compiler can prove that no dynamic dispatch or instance access is needed, a future optimization pass could strip the `_vtable` and `_instance` pointers from the value struct at call sites. This is strictly an **internal codegen optimization** — the C header always emits the full struct layout. The optimizer would insert pack/unpack shims at the boundary if the internal representation differs from the C ABI representation.

This optimization is deferred — it adds complexity and the cost of two extra pointers per value is negligible compared to the benefits of a uniform, no-special-case layout.

---

## Reachability Analysis

Starting from all extern function signatures, walk types transitively to collect everything that needs a C representation. Since we always emit all four struct levels for every type, the analysis only needs to track **which types** are reachable, not which struct levels — simplifying it significantly.

```go
func reachableTypes(file *ast.File, info *sema.Info) []types.Type {
    var queue []types.Type
    visited := map[types.Type]bool{}

    // Seed: all extern function param and return types
    for _, decl := range file.Decls {
        fd, ok := decl.(*ast.FuncDecl)
        if !ok || fd.Body != nil {
            continue  // skip non-extern
        }
        sig := lookupSig(fd, info)
        for _, p := range sig.Params() {
            enqueue(p.Type())
        }
        if sig.Result() != nil {
            enqueue(sig.Result())
        }
    }

    // BFS: for each type, enqueue field types transitively
    for len(queue) > 0 {
        typ := queue[0]; queue = queue[1:]
        if visited[typ] { continue }
        visited[typ] = true

        switch t := typ.(type) {
        case *types.Named:
            for _, f := range t.Fields() {
                enqueue(f.Type())
            }
        case *types.Instance:
            enqueue(t.Origin())
            for _, arg := range t.TypeArgs() {
                enqueue(arg)
            }
        case *types.Array:
            enqueue(t.Elem())
        case *types.Slice:
            enqueue(t.Elem())
        case *types.Map:
            enqueue(t.Key())
            enqueue(t.Val())
        case *types.Tuple:
            for _, e := range t.Elems() {
                enqueue(e)
            }
        case *types.Optional:
            enqueue(t.Elem())
        case *types.SharedRef:
            enqueue(t.Elem())
        case *types.MutRef:
            enqueue(t.Elem())
        case *types.Pointer:
            enqueue(t.Elem())
        }
    }

    return toposort(visited)  // dependency order: leaves first
}
```

The result is topologically sorted so that struct definitions appear before their uses in the header. For each reachable type, all four struct levels (`_t` → `_m` → `_i` → `_v`) are emitted.

---

## Generated Header Format

File: `promise_bindings.h` (generated into a temp directory alongside the `.ll` file)

```c
// Generated by the Promise compiler — DO NOT EDIT
// Source: example.pr
#ifndef PROMISE_BINDINGS_H
#define PROMISE_BINDINGS_H

#include <stdint.h>
#include <stddef.h>

// ============================================================
// Primitive types (four-struct chain, same as all types)
// ============================================================

// int
typedef struct { } promise_int_t;
typedef struct { promise_int_t* _type; } promise_int_m;
typedef struct { promise_int_m* _variant; } promise_int_i;
typedef struct {
    void*           _vtable;
    promise_int_i*  _instance;   // null (no instance data)
    int64_t         raw;
} promise_int_v;

// f64
typedef struct { } promise_f64_t;
typedef struct { promise_f64_t* _type; } promise_f64_m;
typedef struct { promise_f64_m* _variant; } promise_f64_i;
typedef struct {
    void*           _vtable;
    promise_f64_i*  _instance;
    double          raw;
} promise_f64_v;

// bool
typedef struct { } promise_bool_t;
typedef struct { promise_bool_t* _type; } promise_bool_m;
typedef struct { promise_bool_m* _variant; } promise_bool_i;
typedef struct {
    void*            _vtable;
    promise_bool_i*  _instance;
    uint8_t          raw;
} promise_bool_v;

// ... other primitive types follow the same pattern ...

// ============================================================
// Compound types (only those reachable from extern functions)
// ============================================================

// string (data inline in instance via C99 flexible array member)
typedef struct { } promise_string_t;
typedef struct { promise_string_t* _type; } promise_string_m;
typedef struct {
    promise_string_m*  _variant;
    int64_t            len;
    char               data[];
} promise_string_i;
typedef struct {
    void*              _vtable;
    promise_string_i*  _instance;
} promise_string_v;

// Point (all `value fields)
typedef struct { } promise_Point_t;
typedef struct { promise_Point_t* _type; } promise_Point_m;
typedef struct { promise_Point_m* _variant; } promise_Point_i;
typedef struct {
    void*             _vtable;
    promise_Point_i*  _instance;   // null (no instance data)
    double            x;
    double            y;
} promise_Point_v;

// Player (fields across all four struct levels)
typedef struct {
    int64_t maxPlayers;
} promise_Player_t;

typedef struct {
    promise_Player_t*  _type;
    promise_string_v   sprite;
} promise_Player_m;

typedef struct {
    promise_Player_m*  _variant;
    promise_string_v   name;
    int64_t            health;
} promise_Player_i;

typedef struct {
    void*              _vtable;
    promise_Player_i*  _instance;
    double             x;
    double             y;
} promise_Player_v;

// ============================================================
// Extern function declarations
// (all value params passed by pointer; struct returns use sret)
// ============================================================

void promise_print_int(promise_int_v *x);
void promise_print_f64(promise_f64_v *x);
void promise_print_bool(promise_bool_v *x);
void promise_print_string(promise_string_v *s);
void promise_panic(const char* msg);

// User-declared extern functions
// Struct return: void + sret first param
void promise_distance(promise_f64_v *sret, promise_Point_v *a, promise_Point_v *b);
void promise_updateHealth(promise_Player_v *player, promise_int_v *hp);

#endif // PROMISE_BINDINGS_H
```

The header always includes:
1. **All reachable types** — primitives and compound types alike, all four struct levels, in dependency order (`_t` → `_m` → `_i` → `_v`)
2. **Extern function declarations** — all functions with `Body == nil` and `extern` meta, using `promise_T_v*` (pointer) for all params and sret for struct returns

---

## C Symbol Naming Convention

### Functions

The C symbol for an extern function is derived from the Promise function name:

- **Default:** `promise_<funcName>`. E.g., `printInt(int x) \`extern;` → `void promise_printInt(promise_int x);`
- **Explicit override:** `printInt(int x) \`extern("my_print_int");` → `void my_print_int(promise_int x);`

The `extern` meta annotation gains an optional string parameter for the C symbol name. When absent, the `promise_` prefix is applied automatically.

### Types

- Primitives: `promise_<name>` (e.g., `promise_int`, `promise_f64`)
- Named value struct: `promise_<TypeName>_v` (e.g., `promise_Point_v`)
- Named instance struct: `promise_<TypeName>_i` (e.g., `promise_Player_i`)
- Named variant struct: `promise_<TypeName>_m` (e.g., `promise_Player_m`)
- Named type struct: `promise_<TypeName>_t` (e.g., `promise_Player_t`)
- Generic instances: `promise_<TypeName>_<Arg1>_<Arg2>_<suffix>` (e.g., `promise_array_int_v`)
- Enums: `promise_<EnumName>_v` (e.g., `promise_Shape_v`)
- Tuples: `promise_tuple_<T1>_<T2>_v` (e.g., `promise_tuple_int_f64_v`)
- Optional: `promise_optional_<T>_v` (e.g., `promise_optional_int_v`)

---

## Extern Call Codegen

### Current approach (to be replaced)

```
genCallExpr → isRuntimePrintFunc? → genPrintCall (hardcoded coercion)
                                  → regular call (no coercion)
```

### New approach

All extern calls go through a uniform `genExternCall` that handles ABI coercion generically:

```go
func (c *Compiler) genExternCall(ext *ExternFunc, argVals []value.Value, argTypes []types.Type) value.Value {
    var callArgs []value.Value

    // sret: allocate space for the return struct, pass pointer as first arg
    var sretAlloca *ir.InstAlloca
    if ext.HasSret {
        layout := c.lookupLayout(ext.ResultType)
        sretAlloca = c.block.NewAlloca(layout.Value.LLVMType)
        callArgs = append(callArgs, c.block.NewBitCast(sretAlloca, irtypes.I8Ptr))
    }

    for i, arg := range argVals {
        // Ref params: pass the pointer directly
        if isRefType(ext.ParamTypes[i]) {
            callArgs = append(callArgs, arg)
            continue
        }
        // Value params: pack into value struct, alloca, store, pass pointer
        packed := c.packToValueStruct(arg, named, layout)
        alloca := c.block.NewAlloca(layout.Value.LLVMType)
        c.block.NewStore(packed, alloca)
        callArgs = append(callArgs, c.block.NewBitCast(alloca, irtypes.I8Ptr))
    }

    c.block.NewCall(ext.IRFunc, callArgs...)

    // sret: load result from alloca and unpack
    if ext.HasSret {
        result := c.block.NewLoad(layout.Value.LLVMType, sretAlloca)
        return c.unpackFromValueStruct(result, named, layout)
    }
    return nil
}
```

### Coercion rules

The codegen maps Promise internal types to their C ABI representation. All value params are passed by pointer for ARM64 compatibility:

| Internal representation | LLVM IR at call site | C receives | Coercion |
|---|---|---|---|
| Any `T` by value | `alloca` + `store` + `bitcast` → `i8*` | `promise_T_v *x` | pack into value struct, pass pointer |
| `T&` (shared ref) | pass pointer directly | `promise_T_v *x` | no coercion needed |
| `T~` (mut ref) | pass pointer directly | `promise_T_v *x` | no coercion needed |
| Return `T` | sret alloca → `i8*` first arg | `promise_T_v *sret` | load from sret, unpack |

Since all types use the same `promise_T_v` representation at the extern boundary, coercion is uniform. The codegen packs `_vtable`, `_instance`, and user fields into the value struct, stores to an alloca, and passes a pointer. For returns, the sret pattern allocates space, passes a pointer as the first argument, and loads the result after the call.

`unpackFromValueStruct` does the reverse for return values.

**Note on booleans:** Internally Promise uses `i1`, but the value struct stores `uint8_t` (i8) in the `raw` field. The codegen inserts `zext i1 → i8` when packing and `trunc i8 → i1` when unpacking. This is not a special case — it's the normal `raw` field handling.

---

## Build Pipeline Changes

### Current

```
codegen → .ll
clang -c runtime.c → runtime.o
clang program.ll runtime.o → binary
```

### New

```
codegen → .ll + promise_bindings.h
clang -c runtime.c -include promise_bindings.h → runtime.o
clang program.ll runtime.o → binary
```

The key change: `clang` compiles `runtime.c` **with the generated header included**. If the C implementation doesn't match the declared signatures, `clang` produces a compilation error. This catches ABI mismatches at build time, not at runtime.

### Runtime file structure

```
runtime/
├── runtime.c          # Core: print, panic, memory, etc.
├── runtime_string.c   # String operations (stage 8b)
├── runtime_array.c    # Array/slice operations (stage 8g)
└── runtime_task.c     # Concurrency operations (stage 8i)
```

All runtime `.c` files include `promise_bindings.h`. The build pipeline compiles all `.c` files in the runtime directory (or a specified list).

### Cache strategy

The runtime `.o` files only need recompilation when:
1. The generated header changes (extern signatures or reachable types changed)
2. The runtime `.c` source changes

A content hash of the header + source files can be used to cache compiled objects.

---

## Codegen File Changes

### Remove: hardcoded runtime in `builtins.go`

Current `declareRuntime()` with its four hardcoded functions is replaced by a general extern function declaration system.

### New: `compiler/internal/codegen/layout.go`

Computes `TypeDeclLayout` (all four struct levels) for all types reachable from extern signatures. This is the single source of truth for:
- The C header generator (what C struct types to emit)
- The LLVM IR codegen (what LLVM struct types to use for extern calls)
- ABI coercion logic (what conversions are needed at the boundary)

### New: `compiler/internal/codegen/headergen.go`

Generates the `promise_bindings.h` file from the computed layouts.

```go
func GenerateHeader(w io.Writer, layouts map[types.Type]*TypeDeclLayout, externFuncs []*ExternFunc)
```

For each reachable type, emits only the struct levels that are actually needed (tracked by `ReachableType.NeedsValue`, etc.).

### Modified: `compiler/internal/codegen/compiler.go`

```go
func Compile(file *ast.File, info *sema.Info) *CompileResult {
    // ... existing setup ...

    // 1. Collect extern functions
    externs := collectExterns(file, info)

    // 2. Compute layouts for reachable types (all four struct levels)
    layouts := computeLayouts(externs, info)

    // 3. Declare extern functions in LLVM IR (using layouts)
    c.declareExterns(externs, layouts)

    // 4. Declare + define Promise functions (existing)
    c.declareFuncs(file)
    c.defineFuncs(file)

    return &CompileResult{
        Module:  c.module,
        Layouts: layouts,
        Externs: externs,
    }
}
```

The return type changes from `*ir.Module` to a `*CompileResult` that includes the layouts and extern info needed by the header generator.

### Modified: `compiler/internal/codegen/expr.go`

`genCallExpr` no longer special-cases print functions. All extern function calls go through `genExternCall`:

```go
func (c *Compiler) genCallExpr(e *ast.CallExpr) value.Value {
    // ... evaluate args ...

    fn := c.funcs[ident.Name]

    // Check if this is an extern function
    if ext, ok := c.externs[ident.Name]; ok {
        return c.genExternCall(fn, ext.Sig, argVals, argTypes)
    }

    // Regular call (no coercion needed)
    return c.block.NewCall(fn, argVals...)
}
```

### Modified: `compiler/cmd/promise/main.go`

```go
func runBuild(args []string) {
    // ... frontend ...
    result := codegen.Compile(file, info)

    // Write .ll
    writeLL(result.Module)

    // Generate header
    headerFile := writeHeader(result.Layouts, result.Externs)

    // Compile runtime WITH the header
    compileRuntime(headerFile)

    // Link
    link(llFile, runtimeObjs, outputFile)
}

func compileRuntime(headerFile string) {
    // Find all .c files in runtime/
    for _, cFile := range runtimeCFiles {
        clang -c {cFile} -include {headerFile} -o {objFile}
    }
}
```

---

## Handling the `extern` Annotation

### Current grammar

```antlr
funcDecl: IDENT typeParams? LPAREN params RPAREN returnType? metaAnnotation* (block | SEMI) ;
```

An extern function has `Body == nil` (ends with `;`). The `extern` meta is optional in the current grammar.

### Proposed semantics

The `extern` meta becomes **required** for bodyless functions (validated in sema). It optionally takes a string parameter for the C symbol name:

```promise
// Default C name: promise_printInt
printInt(int x) `extern;

// Explicit C name: printf_wrapper
printInt(int x) `extern("printf_wrapper");
```

Sema validates:
- `Body == nil` requires `extern` annotation (error otherwise)
- `extern` annotation requires `Body == nil` (error otherwise)
- The optional string parameter, if present, must be a valid C identifier

### AST change

The `extern` meta's first parameter (if present) stores the C symbol name. Accessed via:

```go
func externCName(fd *ast.FuncDecl) string {
    for _, ann := range fd.Annotations {
        if ann.Name == "extern" && len(ann.Params) > 0 {
            return ann.Params[0].Value  // explicit C name
        }
    }
    return "promise_" + fd.Name  // default
}
```

---

## Passing Convention

All types use the same convention — always `promise_T_v*` (pointer). Struct returns use sret:

| Promise usage | C receives | Notes |
|---|---|---|
| `T` by value | `promise_T_v *x` | Pointer to value struct (alloca'd at call site) |
| `T&` shared ref | `promise_T_v *x` | Pointer to value struct |
| `T~` mutable ref | `promise_T_v *x` | Pointer to value struct |
| Return `T` | `promise_T_v *sret` (first param) | sret pattern; function returns `void` |

C code that needs instance-level data follows `val->_instance->field`. The `_instance` pointer is typed, so this is natural and type-safe.

Passing all value params by pointer avoids ARM64 ABI issues where structs >16 bytes are passed indirectly. The LLVM IR uses `i8*` for value params (with alloca + store + bitcast at the call site), and typed `%promise_T_v*` for ref params.

---

## Migration Path

### Stage 8a (current) → New architecture

1. **Add `layout.go`** — compute `TypeDeclLayout` for primitives only (trivial: single Value with raw field)
2. **Add `headergen.go`** — generate header with primitive aliases + extern declarations
3. **Modify `compiler.go`** — return `CompileResult`, collect externs, compute layouts
4. **Modify `expr.go`** — replace `isRuntimePrintFunc` special case with `genExternCall`
5. **Remove hardcoded runtime from `builtins.go`** — `declareRuntime()` and `declareExternFunc()` are replaced by `declareExterns()`
6. **Update `main.go`** — generate header, compile runtime with `-include`
7. **Update `runtime.c`** — `#include "promise_bindings.h"`, change types to use aliases

### Stage 8b (strings) — Done

- `layout.go`: `computeStringLayout` produces Instance struct `{ variant_ptr, len, [0 x i8] data }` (flexible array member) and Value struct `{ vtable_ptr, instance_ptr }`. No `raw` field.
- `headergen.go` emits `promise_string_i` with C99 flexible array member (`char data[]`) and minimal `promise_string_v`.
- `runtime_string.c` includes the header, implements `promise_string_new`, `promise_string_concat`, `promise_string_eq`, `promise_print_string` using `s._instance->data` and `s._instance->len`.
- String instances allocated with `sizeof(promise_string_i) + len` (no null terminator — strings may contain `\0`).
- `extern.go`: `packString`/`unpackString` coerce `i8*` ↔ `promise_string_v` via bitcast + insertvalue/extractvalue.

### Stage 8c (user types) — Done

- `layout.go`: `computeUserTypeLayout` creates all four struct layouts from Named type fields. Only `PlaceInstance` fields supported (default placement). Instance struct: `{ variant_ptr, field1, field2, ... }`. Value struct: `{ vtable_ptr, instance_ptr }` — no user fields. `InstanceFieldIndex` maps field names to GEP indices. Primitive fields use raw C types; pointer fields use `void*` to avoid forward-declaration issues.
- `headergen.go` emits `_t`, `_m`, `_i`, `_v` struct typedefs for each user type — handled generically by existing `emitStructTypedef`.
- `compiler.go`: `malloc` intrinsic declared. Methods compiled as LLVM functions with mangled names (`TypeName.methodName`), receiver as first `i8*` param. Two-pass: declare stubs, then generate bodies.
- `expr.go`: Constructors via `malloc` + GEP field stores. Field access via bitcast + GEP + load. Method calls via direct dispatch to mangled function. `this` keyword loads from alloca.
- `extern.go`: `packUserType`/`unpackUserType` follow same pattern as strings — `{ null_vtable, bitcast(i8* → T_i*) }`.

### Stage 8g (containers)

- `layout.go` computes array/slice/map layouts — monomorphized, all four levels, with `IsFlexible` for instance structs
- `headergen.go` emits monomorphized container typedefs with flexible array members (e.g., `promise_array_int_i` with inline `data[]`)
- Instance structs use C99 flexible array members for inline element storage, allocated with `sizeof(fixed) + cap * sizeof(element)`
- Growing arrays use `realloc` on the entire instance; ownership system tracks which values need `_instance` pointer updates

---

## Summary

| Concern | Current | Proposed |
|---------|---------|----------|
| C function declarations | Hardcoded in `builtins.go` | Generated from extern AST nodes |
| Type agreement | Trust + manual matching | Generated header verified by clang |
| Four-struct model | Ignored | Every type emits all 4 struct levels |
| Passing convention | Primitives as scalars, special cases | Always `promise_T_v*` (pointer), sret for returns |
| ABI coercion | Per-function special cases | Uniform pack/unpack via `coerceToCABI`/`coerceFromCABI` |
| Compound types | Not supported | `TypeDeclLayout` computation + header generation (flexible array members for inline data) |
| Internal pointers | N/A | Typed (`promise_T_i*`), C navigates naturally |
| Adding new externs | Edit 3 files | Write C implementation, declare in Promise |
| Runtime compilation | `clang -c runtime.c` | `clang -c runtime.c -include promise_bindings.h` |
| Source of truth | Split across Go + C | `TypeDeclLayout` (single, computed) |
