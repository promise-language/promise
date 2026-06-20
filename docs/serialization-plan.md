# Serialization Architecture — Design Plan

Promise needs a serialization system that lets AI agents produce correct serialization code in one shot without memorizing framework-specific APIs. This document proposes an architecture, surveys how other languages handle serialization, and identifies the compiler features needed.

---

## 1. Problem Statement

JSON is the lingua franca of tool-use and API integration. An AI agent building a Promise tool-use program needs to:

1. Parse JSON from an HTTP response into typed Promise structs
2. Serialize typed Promise structs into JSON for API requests
3. Handle field renaming, omission, nested types, enums, and optionals

But JSON is just one wire format. TOML (Promise's own module config), YAML, MessagePack, Protocol Buffers, and custom binary formats all need the same fundamental capability: **inspecting a type's fields at compile time and generating code to read/write them**.

The architecture must support multiple formats without duplicating the field-traversal logic for each one.

---

## 2. Survey: How Other Languages Do It

### 2.1 Go — `encoding/json` + struct tags

```go
type User struct {
    Name  string `json:"name"`
    Email string `json:"email,omitempty"`
    Age   int    `json:"age"`
}
data, _ := json.Marshal(user)
json.Unmarshal(data, &user)
```

**Mechanism**: Runtime reflection (`reflect` package). Struct tags are string metadata on fields. The JSON encoder/decoder uses `reflect.TypeOf` to enumerate fields, read tags, get/set values dynamically.

**Pros**: Zero codegen, works with any type, tags compose across formats.
**Cons**: Slow (runtime reflection), no compile-time validation (wrong tag = silent bug), tag syntax is stringly-typed.

### 2.2 Rust — `serde` + derive macros

```rust
#[derive(Serialize, Deserialize)]
struct User {
    #[serde(rename = "user_name")]
    name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    email: Option<String>,
}
```

**Mechanism**: Procedural macros (`derive`) generate `Serialize`/`Deserialize` trait implementations at compile time. `serde` defines a **data model** (18 types) and two visitor traits. Format libraries (serde_json, serde_yaml, etc.) implement `Serializer`/`Deserializer` against the data model.

**Pros**: Zero runtime cost, compile-time validation, format-agnostic (one `derive` works for JSON, YAML, TOML, MessagePack, etc.), extremely flexible.
**Cons**: Complex (the Serializer/Deserializer traits have 30+ methods), proc macros are opaque, compile times suffer, the error messages from macro expansion are hard to read.

### 2.3 Kotlin — `kotlinx.serialization`

```kotlin
@Serializable
data class User(
    @SerialName("user_name") val name: String,
    val email: String? = null
)
val json = Json.encodeToString(user)
val user = Json.decodeFromString<User>(json)
```

**Mechanism**: Compiler plugin generates a `serializer()` companion method that returns a `KSerializer<T>`. The serializer uses a **descriptor** (field names, types, annotations) and an **encoder/decoder** interface. Format libraries implement the encoder/decoder.

**Pros**: Compile-time, IDE-aware, format-agnostic, clean API. Default values and optionals handled naturally.
**Cons**: Requires a compiler plugin (not a library-only solution), the serialization compiler plugin is complex.

### 2.4 Swift — `Codable`

```swift
struct User: Codable {
    let name: String
    let email: String?

    enum CodingKeys: String, CodingKey {
        case name = "user_name"
        case email
    }
}
let data = try JSONEncoder().encode(user)
let user = try JSONDecoder().decode(User.self, from: data)
```

**Mechanism**: Protocol conformance. The compiler auto-synthesizes `Encodable`/`Decodable` conformance when all fields are Codable. Custom `CodingKeys` enum handles renaming. `Encoder`/`Decoder` protocols abstract the format.

**Pros**: Clean, no macros, format-agnostic, compiler-synthesized.
**Cons**: `CodingKeys` enum is verbose for simple renaming, limited customization without manual `encode`/`init(from:)`.

### 2.5 What Promise Should Take From Each

| From | Take | Adapt |
|------|------|-------|
| **Go** | Field-level metadata annotations for renaming/options | Use typed meta annotations instead of stringly-typed tags |
| **Rust/serde** | Format-agnostic data model with Encoder/Decoder traits | Simplify drastically — Promise has no macros, so use compiler codegen from `` `serializable `` |
| **Kotlin** | Compiler-generated serializer from annotation | Direct inspiration — `` `serializable `` generates methods, not a separate serializer object |
| **Swift** | Protocol-based Encodable/Decodable with auto-synthesis | Model the Encoder/Decoder as structural interfaces |

---

## 3. Proposed Architecture

### 3.1 Core Idea

`` `serializable `` on a type causes the compiler to **generate two methods** on that type:

```promise
encode(Encoder ~e)!       // writes this value's fields into an encoder
decode!(Decoder ~d) Self `factory // reads fields from a decoder, constructs Self
```

These methods call into **format-agnostic `Encoder`/`Decoder` interfaces**. Format libraries (JSON, TOML, etc.) provide concrete implementations of these interfaces. The generated methods describe the type's structure (field names, types, optionals) to the encoder/decoder — they never know which format is being used.

```
┌──────────────────────────────────────────┐
│            User Code                      │
│  type User `serializable { ... }          │
│  json.encode_string(user)!                │
│  json.decode_string[User](data)!          │
└──────────────────┬───────────────────────┘
                   │ calls generated encode()/decode()
                   ▼
┌──────────────────────────────────────────┐
│     Generated encode()/decode()           │
│  e.field("name", this.name)!              │
│  this.name = d.field[string]("name")!     │
└──────────────────┬───────────────────────┘
                   │ calls Encoder/Decoder interface
                   ▼
┌──────────────────────────────────────────┐
│  Encoder/Decoder Implementations          │
│  JsonEncoder, TomlEncoder, ...            │
│  (catalog modules: json, toml, ...)       │
└──────────────────────────────────────────┘
```

### 3.2 The `Encoder` and `Decoder` Structural Interfaces

These live in the standard library (`modules/std/encode.pr`), making them available to all code without importing a format-specific module.

```promise
type Encoder `public
    `doc("Format-agnostic encoding interface. Serialize types by calling methods in sequence.
          Not structural — implementations must declare `is Encoder` explicitly.") {

    // Scalars
    encode_bool!(~this, bool value) `abstract;
    encode_int!(~this, int value) `abstract;
    encode_uint!(~this, uint value) `abstract;
    encode_f64!(~this, f64 value) `abstract;
    encode_string!(~this, string value) `abstract;
    encode_none!(~this) `abstract;

    // Compound — begin/end delimited
    begin_object!(~this, int field_count) `abstract;
    end_object!(~this) `abstract;
    begin_array!(~this, int length) `abstract;
    end_array!(~this) `abstract;

    // Field key in an object context
    encode_key!(~this, string name) `abstract;

    // Encode any Encodable value (dispatches to value.encode(~this))
    encode_value!(~this, Encodable value) `doc("Encodes any Encodable value.") {
        value.encode(this);
    }
}

type Decoder `public
    `doc("Format-agnostic decoding interface. Deserialize types by calling methods in sequence.
          Not structural — implementations must declare `is Decoder` explicitly.") {

    // Scalars
    decode_bool!(~this) bool `abstract;
    decode_int!(~this) int `abstract;
    decode_uint!(~this) uint `abstract;
    decode_f64!(~this) f64 `abstract;
    decode_string!(~this) string `abstract;
    decode_none!(~this) bool `abstract; // returns true if the value is null/none

    // Compound — begin/end delimited
    begin_object!(~this) `abstract;
    end_object!(~this) `abstract;
    begin_array!(~this) int `abstract; // returns element count (-1 if unknown)
    end_array!(~this) `abstract;

    // Field navigation in object context
    next_key!(~this) string? `abstract; // returns next key, none at end
    skip_value!(~this) `abstract; // skip the current value
}
```

**Design notes:**

- The interface is **streaming/event-based**, not tree-based. This means formats that are inherently streaming (MessagePack, Protocol Buffers) don't need to build a tree first.
- The interface is small: ~16 methods per side. Compare with serde's 30+ — we keep it simple because Promise types have fewer representations (signed integers → `encode_int`, unsigned integers → `encode_uint`, floats → `encode_f64` — no per-width methods). Sized types (i8, u16, etc.) route through int/uint with range checking on decode.
- `encode_value` has a default implementation that dispatches to `value.encode(encoder)`, enabling recursive encoding of nested `Encodable` types.

### 3.3 The `Encodable` and `Decodable` Structural Interfaces

```promise
type Encodable `structural `public
    `doc("Types that can serialize themselves to an Encoder.") {
    encode!(Encoder ~e) `abstract;
}

type Decodable `structural `public
    `doc("Types that can deserialize themselves from a Decoder.") {
    decode!(Decoder ~d) Self `factory `abstract;
}
```

- `Encodable` is an instance method — you have a value and encode it.
- `Decodable` is a factory method — you read from a decoder and produce a new value.
- This mirrors the `Format`/`Parse` symmetry already in the standard library.
- `` `serializable `` on a type auto-generates both methods, making the type satisfy both interfaces.

### 3.4 What `` `serializable `` Generates

Given:

```promise
type User `serializable {
    string name `key("user_name");
    int age;
    string? email;
    Address address;
}

type Address `serializable {
    string street;
    string city;
}
```

The compiler generates (conceptually):

```promise
// Auto-generated encode method
encode!(Encoder ~e) {
    e.begin_object!(3);

    e.encode_key!("user_name");
    e.encode_string!(this.name);

    e.encode_key!("age");
    e.encode_int!(this.age);

    // email is T? — omitted when none (default behavior)
    if this.email is present {
        e.encode_key!("email");
        e.encode_string!(this.email);
    }

    e.encode_key!("address");
    this.address.encode!(e); // recursive — Address is Encodable

    e.end_object!();
}

// Auto-generated decode factory
decode!(Decoder ~d) Self `factory {
    d.begin_object!();
    string name = "";
    int age = 0;
    string? email = none;
    Address? address = none;

    while key := d.next_key!() {
        match key {
            "user_name" => name = d.decode_string()!,
            "age" => age = d.decode_int()!,
            "email" => {
                if d.decode_none!() { email = none; }
                else { email = d.decode_string!(); }
            },
            "address" => address = Address.decode(d)!,
            _ => d.skip_value()!,          // ignore unknown fields
        }
    }
    d.end_object!();

    return Self(
        name: name,
        age: age,
        email: email,
        address: address!,   // panic if required field missing — see §3.7
    );
}
```

### 3.5 Field Annotations

Field-level meta annotations control serialization behavior. The defaults are chosen so the common case requires zero annotations — you only annotate the rare case:

| Annotation | Target | Effect |
|-----------|--------|--------|
| `` `key("wire_name") `` | field, variant | Use "wire_name" instead of the field name in the encoded output |
| `` `skip `` | field | Exclude this field from serialization entirely |
| `` `include_none `` | field (T? only) | Force encoding `none` as null instead of omitting the field (default: omit) |
| `` `required `` | field | Raise `DecodeError` if the key is missing during decode, even if the field has a default value |
| `` `flatten `` | field | Inline the nested object's fields into the parent (like serde's `#[serde(flatten)]`) |

**Default behaviors** (no annotation needed):

- **`T?` fields during encode**: omitted when `none`. Most JSON APIs prefer clean output without null fields. Add `` `include_none `` for the rare case where `null` has meaning (e.g., PATCH semantics: `null` means "clear this field").
- **Fields with `= default` during decode**: use the default when the key is missing. The type definition already declares the default — requiring a second annotation is redundant. Add `` `required `` to force an error on missing keys.

```promise
type ApiResponse `serializable {
    int status_code `key("statusCode");
    string? error_message `key("error");     // omitted when none (default behavior)
    string? patch_field `include_none;       // encoded as null when none (PATCH semantics)
    UserData data `flatten;
    string _internal_id `skip;               // never serialized
    int retry_count = 0;                     // uses default 0 if missing (default behavior)
    int version = 1 `required;              // error if missing, despite having a default
}
```

**Why field-level metas instead of Go-style string tags?** Promise's meta annotations are typed and validated at compile time. `` `key("user_name") `` is checked by the compiler — a typo like `` `ky("user_name") `` is a compile error. Go's struct tags are raw strings parsed at runtime — a typo like `` `josn:"name" `` is silent.

### 3.6 Enum Serialization

Enums with `` `serializable `` generate encode/decode that handles variants:

```promise
enum Shape `serializable {
    Circle(f64 radius),
    Rectangle(f64 width, f64 height),
    Point,
}
```

Default enum encoding uses **tagged representation** (a JSON object with a `"type"` discriminator):

```json
{"type": "Circle", "radius": 3.14}
{"type": "Rectangle", "width": 10, "height": 20}
{"type": "Point"}
```

The tag field name defaults to `"type"` but is configurable via `` `serializable(tag: "kind") ``.

Simple enums (no variant data) encode as strings:

```promise
enum Color `serializable {
    Red,
    Green,
    Blue,
}
// Encodes as: "Red", "Green", "Blue"
```

Variant-level `` `key `` renames the discriminator value:

```promise
enum Status `serializable {
    Active `key("active"),
    Inactive `key("inactive"),
}
// Encodes as: "active", "inactive"
```

### 3.7 Required vs Optional Field Handling

During deserialization, the default behavior minimizes boilerplate — fields that *can* be absent *are* absent gracefully:

| Field type | Missing key behavior | Override |
|-----------|---------------------|----------|
| `T` (no default, not optional) | Raise `DecodeError` — truly required | — |
| `T?` (optional) | Set to `none` | — |
| `T` with `= default` | Use the default value | `` `required `` → error on missing |
| `T` with `` `required `` | Raise `DecodeError` even if default exists | — |

During encoding:

| Field type | Encoding behavior | Override |
|-----------|------------------|----------|
| `T` | Always encoded | — |
| `T?` when present | Encoded normally | — |
| `T?` when none | **Omitted** (field not in output) | `` `include_none `` → encode as null |

Unknown keys in the input are silently skipped (via `skip_value()`). This is the safe default for API compatibility — new fields added to a remote API don't break existing clients.

### 3.8 Recursive and Generic Types

`` `serializable `` works with generic types:

```promise
type Wrapper[T] `serializable {
    T value;
    string label;
}
```

The generated `encode` method calls `this.value.encode!(e) ` — which requires `T` to be `Encodable`. The compiler **adds an implicit constraint** `T: Encodable` (for encode) and `T: Decodable` (for decode) on the type parameters of a `serializable` generic type. This is validated at instantiation.

For non-serializable field types, the compiler emits a clear error:

```
type Wrapper[T] is marked `serializable but field 'value' of type T
is not Encodable — add constraint T: Encodable or mark T's type as `serializable
```

### 3.9 Custom encode/decode

A type can be `` `serializable `` and still define custom `encode`/`decode` methods. The user-written method takes precedence over the generated one:

```promise
type Timestamp `serializable {
    int seconds;
    int nanos;

    // Custom: encode as ISO 8601 string instead of {seconds, nanos}
    encode!(Encoder ~e) {
        e.encode_string(this.to_iso8601())!;
    }

    decode!(Decoder ~d) Self `factory {
        string s = d.decode_string!();
        return Self.from_iso8601!(s);
    }
}
```

When the user provides `encode`, the compiler skips generation. When the user provides `decode`, the compiler skips generation. They are independent — you can customize one without the other.

#### Format-specific field names

`` `key `` applies to all formats — it's the wire name for every encoder/decoder. When different formats need different names for the same field (uncommon), don't reach for framework complexity. Write a format-specific helper function instead:

```promise
type Product `serializable {
    string name `key("product_name");   // correct for JSON API
    f64 price;
}

// TOML config uses "title" instead of "product_name" for the same field.
// Write a standalone function — no need to touch the type or the framework:
decode_product_from_toml!(Decoder ~d) Product {
    d.begin_object!();
    string name = "";
    f64 price = 0.0;
    while key := d.next_key!() {
        match key {
            "title" => name = d.decode_string()!,   // TOML-specific key
            "price" => price = d.decode_f64()!,
            _ => d.skip_value()!,
        }
    }
    d.end_object!();
    return Product(name: name, price: price);
}
```

The generated `encode`/`decode` handles the primary format (JSON). The custom function handles the exception (TOML). This is the right trade-off: zero framework complexity for a rare case, and the custom code is explicit about what differs.

---

## 4. The `json` Catalog Module

The `json` module provides `JsonEncoder`, `JsonDecoder`, and convenience functions:

```promise
// modules/json/json.pr

type JsonEncoder is Encoder {
    Builder _buf;

    // Must implement all abstract methods from Encoder.
    // Compile error if any are missing.
    // ... implements all Encoder methods, writing JSON text ...
}

type JsonDecoder is Decoder {
    Scanner _scanner;

    // Must implement all abstract methods from Decoder.
    // Compile error if any are missing.
    // ... implements all Decoder methods, reading JSON text ...
}

// Convenience functions

encode_string[T: Encodable](T value) string!
    `public `doc("Serializes a value to a JSON string.") {
    JsonEncoder enc = JsonEncoder();
    value.encode!(enc);
    return enc.to_string();
}

decode_string[T: Decodable](string data) T!
    `public `doc("Deserializes a value from a JSON string.") {
    JsonDecoder dec = JsonDecoder(data: data);
    return T.decode!(dec);
}

// Pretty-printing variant
encode_string_pretty[T: Encodable](T value, int indent = 2) string!
    `public `doc("Serializes to an indented JSON string.") {
    JsonEncoder enc = JsonEncoder(pretty: true, indent: indent);
    value.encode!(enc);
    return enc.to_string();
}
```

### 4.1 Usage Example

```promise
use json;

type User `serializable {
    string name `key("user_name");
    int age;
    string? email;               // omitted when none (default — no annotation needed)
}

main!() {
    // Encode
    user := User(name: "Alice", age: 30);
    string data = json.encode_string!(user);
    print_line(data);
    // {"user_name":"Alice","age":30}   (email omitted — it's none)

    // Decode
    User parsed = json.decode_string![User](data);
    print_line(parsed.name);   // Alice
    // parsed.email is none — key was missing, T? defaults to none
}
```

### 4.2 Raw JSON (`JsonValue` enum)

For untyped JSON manipulation, the module also provides a `JsonValue` enum:

```promise
enum JsonValue `serializable `public {
    Null,
    Bool(bool value),
    Number(f64 value),
    Str(string value),
    Array(JsonValue[] items),
    Object(map[string, JsonValue] entries),
}

parse(string input) JsonValue!
    `public `doc("Parses a JSON string into a JsonValue tree.");

format(JsonValue value) string
    `public `doc("Formats a JsonValue tree as a JSON string.");
```

This is the escape hatch for dynamic JSON — APIs that return varying shapes, or when you need to inspect JSON before deserializing into a typed struct.

---

## 5. Required Compiler Features

### 5.1 New Built-in Meta Annotations

Add to `builtinMetas` in `sema/meta.go`:

| Meta | Targets | Parameters |
|------|---------|-----------|
| `key` | field, variant | 1 positional string param (the wire name) |
| `skip` | field | none |
| `include_none` | field | none (only valid on `T?` fields — forces null encoding instead of omission) |
| `required` | field | none (error on missing key during decode, even if field has a default) |
| `flatten` | field | none (only valid on `Encodable` fields) |

These are validated during sema. Using `` `key `` on a non-serializable type's field is allowed (it's inert — only read by the serializable codegen).

### 5.2 `` `serializable `` Flag Storage

Add `isSerializable bool` to both `Named` and `Enum` in the type system. Extract during sema's define pass (same pattern as `doc`, `deprecated`, `copy`).

### 5.3 Method Generation in Sema

When a type is marked `` `serializable ``, the sema pass must:

1. Check that all non-`` `skip `` fields have types that are either:
   - Primitive (`int`, `f64`, `bool`, `string`, `char`, `u8`..`u64`, `i8`..`i64`)
   - Optional (`T?`) where `T` is encodable
   - Another `` `serializable `` type or a type implementing `Encodable`/`Decodable`
   - A container of encodable elements (`T[]`, `map[string, V]` where V is encodable)
2. If the user has not defined `encode`, synthesize one
3. If the user has not defined `decode`, synthesize one
4. Add the synthesized methods to the type's method list so they participate in structural satisfaction, vtable generation, and monomorphization

**Implementation approach:** The cleanest approach is to **synthesize AST nodes** for the generated methods during the sema define pass (similar to how auto-generated getters/setters work for vtable slots). This means the methods go through normal type-checking and codegen — no special codegen path needed.

### 5.4 Implicit Constraints on Generic Type Parameters

When a generic type `T[A, B]` is `` `serializable ``, type parameters used in non-`` `skip `` fields need implicit `Encodable`/`Decodable` constraints. The sema pass adds these constraints automatically (or reports an error if the type parameter already has incompatible constraints).

### 5.5 Primitive Encodable/Decodable Implementations

All primitive types need `encode`/`decode` implementations. Since primitives are `` `native `` types, these are added as regular methods:

```promise
// In modules/std/int.pr
type int `native {
    // ... existing ...
    encode!(Encoder ~e) { e.encode_int!(this); }
    decode!(Decoder ~d) Self `factory { return d.decode_int!(); }
}

// In modules/std/uint.pr
type uint `native {
    // ... existing ...
    encode!(Encoder ~e) { e.encode_uint!(this); }
    decode!(Decoder ~d) Self `factory { return d.decode_uint!(); }
}
```

Similarly for `f64`, `bool`, `string`, `char`, and all other primitive types.

**Type routing for sized integers:** The generated code routes each field type to the appropriate encoder method and adds range checking on decode:

- **Signed types** (`int`, `i8`, `i16`, `i32`, `i64`): use `encode_int`/`decode_int`. Subtypes decode via `decode_int()` + bounds check + cast.
- **Unsigned types** (`uint`, `u8`, `u16`, `u32`, `u64`): use `encode_uint`/`decode_uint`. Subtypes decode via `decode_uint()` + bounds check + cast.
- **Floats** (`f32`, `f64`): use `encode_f64`/`decode_f64`. `f32` decodes via `decode_f64()` + cast (precision loss is acceptable — same as JSON).

For example, a field `i8 temperature` generates:

```promise
// Encode: widen to int (always safe)
e.encode_int!(this.temperature as int);

// Decode: narrow with range check
int _raw = d.decode_int!();
if _raw < -128 || _raw > 127 {
    raise DecodeError(message: "value out of range for i8 field 'temperature'", field: "temperature");
}
this.temperature = _raw as i8;
```

A field `u64 snowflake_id` generates:

```promise
// Encode: direct (uint → encode_uint)
e.encode_uint!(this.snowflake_id as uint);

// Decode: direct (decode_uint → uint, then narrow if needed)
this.snowflake_id = d.decode_uint()! as u64;
```

This ensures values above 2^63-1 round-trip correctly through formats that support unsigned 64-bit integers (MessagePack, Protobuf). JSON encoders may choose to emit large uints as strings — that's a format-level decision, not an interface-level one.

Container types (`Vector[T]`, `Map[K,V]`) also need encode/decode:

```promise
// In modules/std/vector.pr (encode for T[])
type Vector[T] {
    // ... existing ...
    encode!(Encoder ~e) {
        e.begin_array!(this.len);
        for item in this {
            item.encode!(e);
        }
        e.end_array!();
    }

    decode!(Decoder ~d) Self `factory {
        int n = d.begin_array!();
        Self result = Self();
        // If n is known, we could pre-allocate
        while !d.end_of_array!() {
            result.push(T.decode(d)!);
        }
        d.end_array!();
        return result;
    }
}
```

### 5.6 `DecodeError` Type

```promise
type DecodeError is error `public {
    string field;    // which field caused the error (empty for root)
    int? position;   // byte position in input (none if unknown)
}
```

---

## 6. Implementation Phases

### Phase 1: Foundation (Encoder/Decoder interfaces + primitives)

**Scope:** Standard library only, no compiler changes.

1. Create `modules/std/encode.pr` with `Encoder`, `Decoder`, `Encodable`, `Decodable` structural interfaces
2. Add `encode`/`decode` methods to all primitive types (`int`, `f64`, `bool`, `string`, etc.)
3. Add `encode`/`decode` to `Vector[T]` and `Map[K,V]`
4. Add `encode`/`decode` to `Optional[T]` (encode as value or null)
5. Create `DecodeError` type
6. Tests: unit tests for each primitive's encode/decode

### Phase 2: JSON Module (hand-written, no codegen)

**Scope:** New catalog module `modules/json/`, no compiler changes.

1. Implement `JsonEncoder` (satisfies `Encoder`) — writes JSON text to a Builder
2. Implement `JsonDecoder` (satisfies `Decoder`) — reads JSON text from a Scanner
3. Implement `JsonValue` enum with `parse`/`format`
4. Implement convenience functions: `encode_string`, `decode_string`, `encode_string_pretty`
5. Tests: JSON parsing/formatting for all JSON types, round-trip tests, edge cases (escapes, unicode, numbers)

**At this point, users can manually implement `encode`/`decode` on their types and use the JSON module.** This validates the Encoder/Decoder interface design before investing in compiler codegen.

### Phase 3: `` `serializable `` Compiler Feature — DONE (primitive fields)

**Scope:** Compiler changes. Implementation: AST synthesis in `sema/serialize.go`.

1. ~~Store `isSerializable` flag on `Named`~~ — **Done** (`types/named.go`)
2. ~~Extract flag during sema define pass~~ — **Done** (`sema/decl.go`)
3. ~~Register new meta annotations (`key`, `skip`, `include_none`, `required`, `flatten`)~~ — **Done** (`sema/meta.go`)
4. ~~Validate field annotations (`include_none` only on `T?` fields)~~ — **Done** (`sema/serialize.go`)
5. ~~Synthesize `encode` method AST if not user-defined~~ — **Done** — handles `key` renaming, `skip` exclusion, optional omission (if-unwrap), `include_none` null encoding
6. ~~Synthesize `decode` factory method AST if not user-defined~~ — **Done** for primitive fields — handles key matching loop, optional null checking, error propagation, `skip` zero-fill, `key` renaming
7. ~~Generic type param constraints~~ — **Done** in Phase 4 with explicit constraints (not implicit)
8. ~~Tests~~ — **Done**: 50 e2e tests in `tests/e2e/serializable_test.pr` covering: encode/decode round-trip, mixed types (string/int/f64/bool), nested types (3 levels, key annotation, multiple nested fields), array fields (`T[]` — string/int/f64/bool/user-type arrays, empty, single element), map fields (`map[K,V]` — string keys, int keys, encode/decode/empty), mixed array+map, field annotations (key/skip/include_none), multiple optionals, string escaping, zero/negative/large values, custom encode override, key renaming in decode

**Known limitations (Phase 3+4):**
- ~~**Nested user-type decode**~~ — **Fixed.** Now uses `T?` local with `!` unwrap in constructor args. Panics if a required nested field is missing from the JSON.
- ~~**Structural interface coercion for user types**~~ — **Fixed.** Synthesized methods use `MutRefTypeRef`. Variable assignment and value type boxing also fixed.
- ~~**Container fields**~~ — **Fixed.** `T[]` encodes as JSON array with inline for-in loop; decodes with `has_next_element` + push loop. `map[K, V]` supports any key type — string keys use directly, non-string keys use `to_string()` for encode and `scan[K]()` for decode (reversible via Format/Parse).
- ~~**Generic serializable types**~~ — **Fixed.** Requires explicit constraints: `Wrapper[T: Encodable + Decodable]`. Unconstrained type params in non-skip fields produce a clear error. Skip fields with TypeParam type are exempt (encode-only, no decode synthesis if constructor can't be satisfied).

### Phase 4: Nested Types, Enums, and Advanced Features

**Scope:** Compiler + standard library.

1. ~~Nested user-type decode~~ — **Done.** Uses `T?` local + `!` unwrap. 3 tests (encode, decode, round-trip).
2. ~~Container field serialization (`T[]`, `map[K, V]`)~~ — **Done.** Inline codegen. Non-string map keys use `to_string()`/`scan[K]()`. Added `has_next_element` to Decoder interface. 9 tests.
3. ~~Enum `` `serializable `` codegen~~ — **Done.** Tag-based for data enums, string for simple enums. 52 tests.
4. ~~`` `flatten `` support~~ — **Done.** Inlines nested type fields into parent during encode/decode. Validates type, annotation conflicts, and wire name collisions. 9 e2e tests + 8 sema tests.
5. ~~`` `serializable(tag: "kind") `` parameter~~ — **Done.** Custom discriminator field name (default `"type"`). 7 tests.
6. Nested generic serialization (`Wrapper[User]`, `map[string, User[]]`)
7. ~~Generic type parameter validation~~ — **Done.** Explicit constraints required (`T: Encodable + Decodable`). Clear error for unconstrained params. Skip fields exempt. 7 tests (int, string, bool, f64, user type, round-trip, skip).
8. Tests: nested round-trip, container round-trip, enum round-trip, flatten, custom tags

### Phase 5: Additional Format Modules (future)

Not part of this plan, but the architecture supports:

- `modules/toml/` — TOML encoder/decoder (can reuse existing `promise.toml` parsing)
- `modules/yaml/` — YAML encoder/decoder
- `modules/msgpack/` — MessagePack binary encoder/decoder

Each module only needs to implement `Encoder`/`Decoder`. All `` `serializable `` types work automatically.

---

## 7. Design Decisions & Rationale

### 7.1 Why Compiler Codegen Instead of Runtime Reflection?

Promise has no runtime reflection system and shouldn't need one. The four-struct model stores RTTI for inheritance checks, but not for field enumeration. Adding runtime reflection would:

- Bloat binaries (field name strings, type info for all fields)
- Slow down serialization (dynamic dispatch on every field)
- Contradict Promise's "explicit, no hidden effects" philosophy

Compiler-generated methods are:
- Zero overhead (direct field access, inlined by LLVM)
- Validated at compile time (wrong field types caught early)
- Inspectable (the generated code is just Promise methods)

### 7.2 Why Streaming Encoder/Decoder Instead of Tree-Based?

A tree-based approach (build a `JsonValue`, then serialize the tree) requires an intermediate allocation for every serialization. The streaming approach writes directly to the output:

```promise
// Tree-based (wasteful):
json_tree := JsonValue.Object({"name": JsonValue.Str("Alice"), ...});
output := json_tree.format();

// Streaming (direct):
encoder.begin_object!(2);
encoder.encode_key!("name");
encoder.encode_string!("Alice");
encoder.end_object!();
```

The streaming model also naturally extends to binary formats (MessagePack, Protobuf) where there is no tree to build.

Users who need tree manipulation can always use `JsonValue` directly — the streaming encoder/decoder and the tree-based `JsonValue` are complementary, not competing.

### 7.3 Why Non-Structural for Encoder/Decoder, Structural for Encodable/Decodable?

`Encoder` and `Decoder` have ~12 abstract methods each. They are **not** structural:

- **Few implementations** — JSON, TOML, MessagePack, maybe a handful more. These are always intentional, never accidental.
- **Explicit `is` forces completeness** — `type JsonEncoder is Encoder {}` is a compile error if any abstract method is missing. With structural, you'd only discover the gap when something tries to pass it where `Encoder` is expected.
- **No view vtable overhead** — single-parent `is` means the child's vtable is prefix-compatible with the parent's. No per-view vtable generation needed.

`Encodable` and `Decodable` have 1 abstract method each. They **are** structural:

- **Widely satisfied** — every serializable type, every primitive, every container. Requiring `is Encodable` on `int`, `string`, `Vector[T]`, etc. would be noisy.
- **Accidental satisfaction is harmless** — if a type happens to have `encode!(Encoder ~e) `, it *should* be encodable.

### 7.4 Why AST Synthesis Instead of IR Generation?

Generating the encode/decode methods as AST nodes (rather than generating LLVM IR directly) means:

- They go through normal type-checking — bugs in the generator are caught by sema
- They participate in normal monomorphization — generics Just Work
- They're visible in `promise doc` output
- The implementation stays in sema, not scattered across codegen

The downside is that AST synthesis is a new pattern in the compiler (current codegen for structural default methods operates on existing AST nodes from the source code). But this is a bounded addition — the generated AST is formulaic and doesn't require new AST node types.

### 7.5 Comparison with the Format/Parse Pair

The serialization system parallels the existing Format/Parse design:

| | Format (display) | Parse (text) | Encodable (structured) | Decodable (structured) |
|---|---|---|---|---|
| Interface | `Format` | `Parse` | `Encodable` | `Decodable` |
| Sink/Source | `Writer` (bytes) | `Reader` (bytes) | `Encoder` (typed events) | `Decoder` (typed events) |
| Method | `format!(Writer ~w)` | `parse!(Reader ~r) Self` | `encode!(Encoder ~e)` | `decode!(Decoder ~d) Self` |
| Method kind | Instance | Factory | Instance | Factory |
| Auto-generated | No | No | Yes (`` `serializable ``) | Yes (`` `serializable ``) |
| Format-specific | No (plain text) | No (plain text) | Yes (JSON, TOML, ...) | Yes (JSON, TOML, ...) |

Format/Parse is for **human-readable text** (stdout, logs, debug display). Encodable/Decodable is for **machine-readable structured data** (APIs, config files, wire protocols). They are orthogonal — a type can implement both.

---

## 8. Open Questions

### 8.1 Should `Encodable`/`Decodable` be two interfaces or one `Serializable`?

**Current proposal: two.** Rationale: some types are encode-only (you never deserialize them) or decode-only (you never serialize them). Separate interfaces give fine-grained control. The `` `serializable `` annotation generates both, but a user can implement just one.

### 8.2 How should `map[K, V]` serialize when K is not string?

**Resolved: convert via Format/Parse.** Non-string keys are converted to string via `to_string()` for encoding and parsed back via `scan[K]()` for decoding. This leverages the existing `Format`/`Parse` structural interfaces — any type that can format to string and parse from string can be a map key. String keys are used directly (no conversion overhead). The `Encoder` interface has `encode_key(string)`, so all keys must ultimately become strings at the wire level.

### 8.3 Should unknown keys during decode be an error or silently skipped?

**Current proposal: silently skipped.** This matches JSON API convention (forward-compatible). A future `` `serializable(strict: true) `` parameter could change this to error on unknown keys.

### 8.4 AST synthesis vs a dedicated codegen path?

**Resolved: AST synthesis.** The Phase 3 implementation (`sema/serialize.go`) synthesizes `MethodDecl` AST nodes during sema pass 2. These flow through normal type-checking (pass 3), return analysis (pass 4), and codegen — no special codegen path needed. The approach works well for primitive and optional fields. The synthesized AST is formulaic (if-unwrap for optionals, call-member for encode/decode, for-loop for key matching) and uses existing AST node types.

### 8.5 Naming: `encode`/`decode` vs `serialize`/`deserialize`?

`encode`/`decode` is shorter and follows Promise's full-word convention better than `ser`/`de`. It also avoids confusion with `Format`/`Parse` (which could be called "serialization" colloquially). The interfaces are `Encodable`/`Decodable`, not `Serializable`/`Deserializable`, because the meta annotation `` `serializable `` already uses "serializable" — using the same word for both the annotation and the interface would be confusing.

---

## 9. Summary

| Component | Location | Phase | Status |
|-----------|---------|-------|--------|
| `Encoder`/`Decoder` interfaces | `modules/std/encode.pr` | 1 | **Done** |
| `Encodable`/`Decodable` interfaces | `modules/std/encode.pr` | 1 | **Done** |
| Primitive encode/decode | `modules/std/int.pr`, etc. | 1 | **Done** — all 17 types with range checking |
| Container encode/decode | `sema/serialize.go` (inline codegen) | 4 | **Done** — `T[]` and `map[K,V]` via inline loops |
| `DecodeError` | `modules/std/encode.pr` | 1 | **Done** |
| `JsonEncoder`/`JsonDecoder` | `modules/json/json.pr` | 2 | **Done** — 61 tests |
| `JsonValue` enum | `modules/json/json.pr` | 2 | **Done** — parse_value, format_value, encode_value, 27 tests |
| `json.encode_string`/`json.decode_string` | `modules/json/json.pr` | 2 | **Done** |
| `` `serializable `` flag in types | `types/named.go` | 3 | **Done** |
| Field annotations (`key`, `skip`, `include_none`, `required`, `flatten`) | `sema/meta.go`, `sema/decl.go` | 3 | **Done** |
| AST synthesis for encode/decode | `sema/serialize.go` | 3 | **Done** — primitives, optionals, nested, containers |
| Nested user-type decode | `sema/serialize.go` | 4 | **Done** — `T?` local + `!` unwrap |
| Enum serialization | `sema/serialize.go` | 4 | Planned |
| `` `flatten `` support | `sema/serialize.go` | 4 | **Done** |
| TOML/YAML/MsgPack modules | `modules/toml/`, etc. | 5 | Planned |
