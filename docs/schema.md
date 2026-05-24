# Schema — Type-Driven Schema Generation

`modules/schema` provides a compile-time descriptor for any Promise declaration:
its fields, methods, generic arguments, optionality, defaults, and a stable 128-bit
content-addressed identity. The descriptor is the shared substrate for several
unrelated consumers:

- **AI tooling** (`docs/ai-platform.md`) — JSON Schema for LLM tool definitions,
  structured-output validation, MCP server registration.
- **Cloud persistence** (`docs/cloud-persistence.md`) — wire-stable identity for types,
  fields, and references; server-side validation; schema evolution across versions.
- **Documentation tooling** — `promise doc`, IDE introspection, OpenAPI export.

Schema lives in its own doc because none of those consumers should own it. It is
built on top of the same sema pass that synthesizes encode/decode for
`` `serializable `` types (`docs/serialization-plan.md`) — there is no separate
reflection mechanism.

---

## 1. Design Constraints

The schema descriptor must faithfully model what Promise actually expresses, not what
a generic JSON-Schema library expresses. Concretely:

1. **Optional vs required must match Promise's three-state field model.** Promise has
   *three* presence states for a field, not two. A descriptor that only carries
   `bool required` collapses two of them and produces wrong wire output:

   | Promise field                         | Optional? | Has default?  | Decode if missing | Encode when none  |
   |---------------------------------------|-----------|---------------|-------------------|-------------------|
   | `string name;`                        | no        | no            | error             | always encoded    |
   | `string? email;`                      | yes       | implicit none | set to none       | omitted (default) |
   | `string role = "viewer";`             | no        | yes           | use default       | always encoded    |
   | `int version = 1 `` `required ``;`    | no        | yes (overridden) | error          | always encoded    |
   | `string? v `` `include_none ``;`      | yes       | implicit none | set to none       | encoded as null   |

   The schema must capture all five rows — JSON Schema's `required` array is just a
   downstream rendering concern.

   `` `required `` is only meaningful when the field would otherwise have a fallback
   (a `=` default, or `T?` defaulting to `none`). Applying it to a plain non-optional
   non-defaulted field is a no-op — the field is already required by definition.

2. **Names and docs come from `` `doc ``, not from heuristics.** The compiler already
   stores `` `doc `` text on every type, field, method, function, parameter, and enum
   variant (language-design §8.4). The schema must wire those through verbatim.

3. **Field-level serialization metas must be honored.** A field marked `` `key("foo") ``
   serializes as `"foo"`, not as the Promise field name. A field marked `` `skip ``
   must not appear in the schema. A field marked `` `flatten `` must inline its nested
   object's properties into the parent. These metas already exist for `` `serializable ``
   and the schema must agree with what the generated `encode`/`decode` actually emits.

4. **One descriptor must cover types, methods, free functions, and getters.** MCP
   servers expose tools (functions) and resources (typed handlers); structured output
   uses types; cloud persistence uses types; agent introspection wants to enumerate
   methods. One descriptor shape with seven kinds covers all of this without
   duplication.

5. **Identity must be wire-stable and content-addressed.** Renaming a field, adding a
   parameter, or changing a generic argument produces a different identity unless the
   author explicitly pins the old one. Identity is a 128-bit hash with deterministic
   inputs (§4) so two compilers, two compiler versions, or two languages reading the
   same Promise source agree on what `Foo.bar` is.

6. **Generation must be available in any module, not just `std`.** Catalog modules,
   community modules, and user projects all produce schemas the same way (§7).

---

## 2. The `Type` Tagged Enum

The descriptor for one Promise declaration is a tagged enum named `schema.Type`.
"Type" is the schema-language convention (Object / Array / Function / … are the
"types" of a schema), not Promise's `type` keyword — at the call site, `schema.Type`
disambiguates clearly. Because all symbols are namespaced under `schema.`, the helper
structs drop the `Schema` prefix entirely: `schema.Field`, `schema.Variant`,
`schema.Param`.

A tagged enum is the right shape because every consumer needs to discriminate on the
kind anyway. A Promise enum makes that discrimination exhaustive (the compiler errors
on missing match arms), removes the long list of nullable fields-per-kind that a
struct-with-`kind`-field would need, and matches how the standard library models
comparable shapes (`json.JsonValue`).

```promise
// modules/schema/schema.pr

// Promise scalar primitives.
enum ScalarKind `public {
    Bool, Char, String,
    Int, Int8, Int16, Int32, Int64,
    Uint, Uint8, Uint16, Uint32, Uint64,
    F32, F64,
}

// 128-bit content-addressed identity. See §4.
// Wraps a single native u128 (see docs/large-integers.md); the wrapper carries
// the semantic name and the `value/`clone placement so it stays distinct from
// a plain numeric u128 at the type system level.
type Hash128 `public `value `clone {
    u128 value;
}

// Where a type is defined. Determines the project-level inputs to the hash. See §5.
enum Origin `public `clone {
    Embedded(string module),                    // catalog: "std", "json", "ai" — name pinned in catalog.toml
    External(string url),                       // community module: identified by url alone (commit is a catalog.toml pin, not identity)
    Project(Hash128 project_id),                // current project — id from promise.toml (§6)
}

// The schema descriptor for one Promise declaration. One enum, one match expression
// at every consumer — no `kind` field, no nullable per-kind payloads.
enum Type `public `clone `serializable
    `doc("Compile-time descriptor of a Promise type, function, or method.") {

    // Heap user types marked `serializable. Includes inherited and flattened fields,
    // excludes `skip-ed fields. `definitions` carries sub-schemas for cycles and shared types.
    Object(
        Hash128 id,                              // see §4
        Origin origin,                           // see §5
        string name,                             // source name, used for diagnostics + JSON Schema title
        Type[] type_args,                        // generic instantiation, in declaration order; empty for non-generic
        string? description,                     // from `doc on the declaration
        Field[] fields,
        map[Hash128, Type] definitions,
    ),

    // T[] — element schema only; the array itself is anonymous.
    Array(Type element),

    // map[K, V]
    Map(Type key, Type value),

    // Primitives. `description` is filled when the scalar is a named field.
    Scalar(ScalarKind kind, string? description),

    // Both simple enums (Variant.fields is empty) and tagged-data enums.
    // discriminator_key is "type" by default and is overridden via `serializable(tag: "kind").
    Enum(
        Hash128 id,
        Origin origin,
        string name,
        Type[] type_args,
        string? description,
        Variant[] variants,
        string discriminator_key,
    ),

    // Free function or method declaration. Captures parameter names and per-parameter `doc,
    // which only declarations carry — function-typed values erase both (see §3).
    Function(
        Hash128 id,
        Origin origin,
        string name,
        string? description,
        Param[] parameters,
        Type? return_type,
        bool failable,                          // declared with `!`
    ),

    // T? wrapper. Inner carries the wrapped Type. No identity of its own — identity flows
    // through the inner Type.
    Optional(Type inner),

    // Named reference into the enclosing Object's `definitions` map. Emitted on cycles
    // and on widely-shared sub-types so the same descriptor is not duplicated.
    Reference(Hash128 id),
}

type Field `public `clone `serializable {
    Hash128 id;                              // h(parent_type.id, name) by default; see §4
    string name `doc("Wire name — already adjusted for `key annotations.");
    string source_name `doc("Original Promise field name (for diagnostics).");
    Type field_type;
    string? description;

    // Presence semantics — see §1 table. Exactly one of these three is true for any field.
    bool required `doc("Field must be present during decode.");
    bool optional `doc("Field type is T?. None is the implicit value when missing.");
    bool has_default `doc("Field declares an `= expr default.");

    string? default_repr `doc("Source-form repr of the default expression, if has_default.");
    bool include_none_in_output `doc("True for T? fields with `include_none — encode as null instead of omitting.");
    bool readonly `doc("True for `final fields — informational only.");
}

type Variant `public `clone `serializable {
    Hash128 id;
    string name;             // wire name (already adjusted for `key on the variant)
    string source_name;      // Promise variant name
    string? description;     // from `doc on the variant
    Field[] fields;          // empty for variants with no payload
}

type Param `public `clone `serializable {
    Hash128 id;
    string name;             // parameter name from the function signature
    Type param_type;
    string? description;     // from `doc on the parameter
    bool optional;           // parameter type is T?
    bool has_default;        // parameter has `= expr`
    string? default_repr;
}
```

### Construction — free functions

Promise enum methods cannot be `` `mono `` or `` `factory `` (language-design §5.6).
Both constructors are therefore plain module-level functions in `schema`:

```promise
// modules/schema/schema.pr (continued)

of[T]() Type `mono `public
    `doc("Compile-time derivation from a Promise type. T must be marked `serializable.");

for_func[F]() Type `mono `public
    `doc("Compile-time derivation from a free-function or method declaration.");
```

Call sites:

```promise
schema.Type t  = schema.of[CreateUserRequest]();
schema.Type fn = schema.for_func[read_text]();
```

### Rendering — methods on `Type`

Renderers are normal methods on the enum. A consumer that adds a new render target
gets a compile error from sema if it forgets a variant — no quiet fallthrough.

```promise
type Type {
    to_json_schema(this) string `public `doc("Render as draft-2020-12 JSON Schema.") {
        return match this {
            Type.Object(_, _, name, _, desc, fields, defs) => _render_object_json(name, desc, fields, defs),
            Type.Array(elem)                               => _render_array_json(elem),
            Type.Map(k, v)                                 => _render_map_json(k, v),
            Type.Scalar(kind, desc)                        => _render_scalar_json(kind, desc),
            Type.Enum(_, _, name, _, desc, vs, tag)        => _render_enum_json(name, desc, vs, tag),
            Type.Function(_, _, name, desc, ps, r, f)      => _render_function_json(name, desc, ps, r, f),
            Type.Optional(inner)                           => inner.to_json_schema(),     // emit nullable wrapper
            Type.Reference(id)                             => "{\"$ref\": \"#/$defs/{id.to_hex()}\"}",
        };
    }

    to_openapi(this) string `public `doc("Render as OpenAPI 3.1 schema fragment.");
    to_tool_input_schema(this) string `public `doc("Render as the Anthropic/OpenAI tool input_schema shape.");
}
```

---

## 3. Functions vs Function Types

A subtle but load-bearing distinction:

- **Function-typed values** — e.g., a parameter of type `(string, int) -> bool`. The
  type erases parameter names, per-parameter `` `doc ``, and defaults (language-design
  §9.5). Only positional types and the return type survive. `schema.for_func` does
  **not** accept these.
- **Function declarations** — e.g., a free function `add(int a, int b) int`. The
  declaration carries names, parameter `` `doc ``, defaults, return type, and
  failability. `schema.for_func[add]()` resolves to the declaration (the identifier
  path), not a function value, and recovers all of it.

This is why MCP server authoring (`docs/ai-platform.md` §6) and `promise ai serve`
work: both register free functions or methods, so the declaration is in scope and the
names/docs are available. They never derive schemas from anonymous closures.

---

## 4. Identity — `Hash128`

Identity is the part of the schema that other systems pin to. JSON renderers can
ignore it; cloud persistence and cross-version wire formats cannot. The hash inputs
are fully specified so two compilers, two compiler versions, or two languages reading
the same Promise source produce the same `Hash128`.

### 4.1 Composition

Identity is composed bottom-up:

| Construct | Default hash inputs |
|-----------|---------------------|
| Type (`Object`, `Enum`, `Function`)    | `h(origin, source_name, [arg.id ...])` where `arg.id` is the recursive hash of each generic type argument in declaration order |
| Field, Variant, Param                  | `h(parent_type.id, source_name)` |
| `Reference(id)`                        | the referenced construct's id verbatim |
| `Optional`, `Array`, `Map`             | no identity of their own — identity flows through the inner element(s) |

`origin` serializes deterministically (see §5). The hash function is a 128-bit
content hash; the exact algorithm is implementation-defined but must be stable across
compiler versions once chosen — changing it is a hard schema break.

`type_args` is the recursive component: `Vector[int]` and `Vector[string]` get
different ids because their argument lists differ. `Map[string, Vector[int]]` flattens
to two argument hashes (`string`, `Vector[int]`), in that order.

Renames change the default hash. That is the correct default — most renames *are*
breaking changes — and the override below handles the cases where they aren't.

### 4.2 The `` `id `` Override

A new built-in meta annotation pins the identity of a declaration, allowing the
source name to change while the wire identity stays put.

| Meta | Applies To | Description |
|------|------------|-------------|
| `` `id("<32-hex>") `` | types, enums, fields, methods, variants, parameters | Pin this construct's `Hash128`. The compiler does not mix the source name in. |

```promise
type User `serializable `id("a3f1b27c4d8e91035e2b8c7d4f1a09e60") {
    string display_name `id("0c8e5a2b9f4c771b3a8d6e0291c5f4b7");   // formerly `name`
    string created_at;
    int? age = 0;
}
```

Rules:

- The argument is a 32-character lowercase hex literal (16 bytes = 128 bits). The
  compiler validates length and charset.
- When present, it **replaces** the computed hash. Source name is not mixed in.
- Pinning a declaration does not propagate to its children. A type pinned via
  `` `id `` still gives its fields default-derived ids (using the pinned parent id
  plus the field name). Field-level renames need their own field-level `` `id ``.
- Two declarations with the same pinned `id` is a compile error — the hash space is
  flat.

The annotation is generic on purpose. The same identity flows through cloud
persistence, MCP wire format, schema evolution, and any future content-addressed
import system; naming the meta `` `cloud_id `` would falsely tie one consumer to the
mechanism.

### 4.3 Rename and Deprecation

Pinning carries forward, never backward. If a field is **deleted**, its `Hash128`
goes with it; data already-in-the-cloud under that hash becomes orphaned. That is a
server-side migration concern, not a language feature — see
`docs/cloud-persistence.md` §9 for the recommended deprecation flow.

### 4.4 The `` `entity `` Annotation

A second built-in meta extends the schema model from "describable values" to
"separately-stored entities":

| Meta | Applies To | Description |
|------|------------|-------------|
| `` `entity `` | types | The type is a separately-stored cloud row, not an inlined value. Implies `` `serializable ``. The compiler synthesizes a hidden `Hash128` id field, exposed via an `id` getter and a constructor parameter. References to entity-typed fields encode as id-only on the wire (instead of being inlined). |

```promise
type Address `serializable {        // inlined wherever it appears
    string street;
    string city;
}

type Item `entity {                 // separate cloud row
    string title;
    Address address;                // inlined into Item's encoded data
    Ref[Folder]? folder;            // reference — encodes as id only
}

type Folder `entity {
    string name;
    Inverse[Item] items `inverse(Item.folder);
}
```

Rules:

- `` `entity `` implies `` `serializable ``. Writing both is allowed (and a no-op);
  writing `` `entity `` alone is the recommended form.
- The synthesized id field has the same `Hash128` semantics as any other field id —
  it can be pinned with `` `id `` if the type is renamed without breaking storage.
- Construction normally omits the id (`Item(title: "x", ...)`), letting the cloud
  module assign one on first `Put`. Client-id workflows pass it explicitly:
  `Item(id: my_uuid, title: "x", ...)`. See `docs/cloud-persistence.md` §4.2.
- Schema-side, an `` `entity ``-typed field of another type is encoded as
  `Type.Reference(target_entity_type_id)` — distinguishing it from an inlined
  `` `serializable `` value type which encodes as a full `Type.Object`.

`` `entity `` is a schema concern (it changes the wire shape and the descriptor
emitted by the compiler), which is why the annotation lives in this doc. Its
runtime semantics — what `Ref[T]` does, how lazy fetching works, how inverse
relations are maintained — belong to the cloud-persistence module
(`docs/cloud-persistence.md` §6).

---

## 5. The `Origin` Enum

`Origin` identifies the project a declaration was defined in. It is one of the
inputs to a type's `Hash128`, so its wire form must be deterministic.

```promise
enum Origin `public `clone `serializable {
    Embedded(string module),                    // "std", "json", "ai" — catalog name
    External(string url),                       // community module — url alone (commit is a catalog.toml pin, not identity)
    Project(Hash128 project_id),                // user's own project — see §6
}
```

| Variant | Hash input |
|---------|-----------|
| `Embedded(name)`            | `h("embed", name)` |
| `External(url)`             | `h("git", url)` |
| `Project(project_id)`       | `project_id` directly (already a 128-bit value) |

`Project` collapses local-project identity into a single `Hash128`, regardless of
whether the user pinned it via `id =` or it was derived from `url =` (§6). Downstream
consumers see one shape.

---

## 6. Project Identity — `promise.toml`

A project's hash is the leaf input for every `Hash128` of every type defined in that
project. The project must therefore have a stable identity *if* anything in it
produces or consumes a hash. Most executables don't — and they shouldn't pay any
cost.

### 6.1 The `[executable]` Table

Catalog modules already use `[module]` in `promise.toml`. Executables get a parallel
`[executable]` table:

```toml
# library module — unchanged
[module]
name = "json"

# executable — new
[executable]
name = "my_app"
# id / url optional — only consulted when a hash is actually needed (§6.2)
```

`[module]` and `[executable]` are mutually exclusive. One toml = one role; carrying a
discriminator inside `[module]` would just push the same distinction into a different
field while losing the typed split. Future per-role fields (entry point, sandbox
defaults, deploy target) can land in `[executable]` without overloading `[module]`.

### 6.2 Lazy Resolution of Project Identity

The compiler computes a project hash **only when something actually needs one**.
"Something" means: a `` `serializable `` type is defined in the project, OR
`schema.of[T]()` is called on a project-local type, OR a `` `tool ``-annotated
function exists. A pure CLI that reads files and prints output never triggers a
project-hash computation.

When the hash is needed, resolution proceeds in this order:

1. `[executable] id = "<32-hex>"` → use it directly. Stable across machines and builds.
2. else `[executable] url = "..."` → `project_id = h("git", url)`. Stable as long as
   the project URL is stable.
3. else `[executable] name = "..."` → `project_id = h("name", name)`, with a one-shot
   compile warning:
   ```
   warning: project hash for 'my_app' is derived from `name` because no `id` or `url`
            is set in [executable]. Schemas / cloud-persisted data will break if the
            project is renamed.
            run `promise id --set` to freeze a stable id, or set `url` if the project
            is published.
   ```

This means:

- **Pure CLI / script** — nothing in the toml beyond `name`, no warning, no hash.
- **Schema-emitting executable that doesn't care about cross-build stability** —
  works on `name` alone, gets the warning the first time it builds.
- **Cloud-persistent or wire-stable executable** — adds `id =` once (or `url =` if
  published). Warning gone. Hash now stable across machines and builds.

`promise init` does **not** auto-generate `id` by default — the warning is the prompt
to set one when the user actually needs stability.

### 6.3 Module Identity

Library modules in `[module]` already have a stable identity by virtue of how they
are loaded:

- **Embedded catalog modules** — identified by the catalog name (`"std"`, `"json"`).
  No extra config needed; the name is reserved in `catalog.toml`.
- **External community modules** — identified by `url` alone, pinned in the
  consumer's `catalog.toml` entry. Only `url` participates in the hash.

The `commit` in `catalog.toml` is a **version pin**, not part of the identity. It
tells the build system which revision of the module to fetch; every type and field
defined in that module keeps the same `Hash128` regardless of which commit is
pinned. Including `commit` in identity would invalidate every type's hash on every
upstream commit — the opposite of what schema evolution needs.

The same logic that lets a project rename a field without breaking storage (via
`` `id `` in §4.2) lets a community module evolve its source without breaking
consumers: as long as the `url` is stable and the field-level `Hash128`s are
preserved through pinning where needed, the whole module's identity persists across
commits.

There is no library equivalent of `[executable] id =`. A library cannot exist without
being importable, and importability already requires either a catalog name or a
`url`. Adding `id` to `[module]` would just be a third way to spell the same thing.

---

## 7. Compiler Extension — How Generation Works

`schema.of[T]()` and `schema.for_func[F]()` are `` `mono `` free functions: each
instantiation triggers the compiler's monomorphization pipeline (CLAUDE.md
§"Monomorphization"). The schema-generation hook runs in the same sema pass that
synthesizes encode/decode for `` `serializable `` types
(`compiler/internal/sema/serialize.go`). It produces an AST-level expression that
constructs the `Type` value at runtime.

### 7.1 What the Compiler Must Do

For every `` `serializable `` type, while synthesizing encode/decode methods:

1. **Synthesize a hidden factory** `_schema_descriptor() Type `` `factory `` `` `mono ``
   `` `internal ``` that returns the runtime `Type` value. The method is private (uses
   `_` prefix); only the `schema` module accesses it via a compiler-recognized
   intrinsic.
2. **Compute the type's `Hash128`** using §4.1 inputs. If the declaration carries
   `` `id("...") ``, use the pinned value instead.
3. **Compute every field's `Hash128`** using §4.1 (`h(parent.id, field.source_name)`)
   or honoring per-field `` `id `` overrides.
4. **Capture the Origin** — `Embedded(name)` for catalog modules, `External(url)`
   for community modules, `Project(project_id)` for the current project. The
   project_id is the value resolved per §6.2 (and the one-shot warning fires here on
   first use within the build).
5. **Recurse into field types** — for `Object` and `Enum` field types that are also
   `` `serializable ``, emit an inline `Type.Object`/`Type.Enum` value or, on cycles,
   a `Type.Reference(id)` pointing into the parent object's `definitions`.

For free-function and method declarations carrying `` `tool `` (or any other
schema-relevant annotation that becomes recognized later), the compiler also emits a
parallel `_func_schema_descriptor()` factory that captures parameter names,
per-parameter `` `doc ``, defaults, return type, and failability.

### 7.2 Scope

The hook runs for any `` `serializable `` type in any module (std, catalog,
community, project). This is required so:

- A user project can define `type Request `` `serializable `` { ... }` and call
  `schema.of[Request]()` to drive an MCP server or a cloud-persisted store.
- A community module can define types whose schemas are visible to consumers.

### 7.3 Why Not a Separate `` `schemable `` Annotation?

Two annotations meaning "describe my fields" is bad ergonomics. A type that wants to
be described to an LLM almost always also wants to round-trip JSON. The few cases
where that is not true (a type that should be visible but never serialized) are
handled by `schema.for_func` over a constructor, or by a manually-built
`schema.Type` literal.

---

## 8. Worked Examples

### 8.1 Type Schema with Default Identity

```promise
use schema;

type CreateUserRequest `serializable
    `doc("Request to create a new user.") {
    string name `doc("The user's full name.");
    string email `doc("Email address. Must be unique.") `key("email_address");
    int? age `doc("Age in years. Optional.");
    string role = "viewer" `doc("Role assignment.");
    string _trace_id `skip;
    int version = 1 `required `doc("Schema version — required even though defaulted.");
}

main!() {
    schema.Type s = schema.of[CreateUserRequest]();
    print_line(s.to_json_schema());
}
```

The descriptor honors `` `key("email_address") `` (wire name in `properties` and
`required`), `` `skip `` (`_trace_id` absent), `T?` (`age` not required), `=`
default (`role` has `default`, not required), and `` `required `` (`version` has
`default` *and* is required). Identity is auto-derived from the project origin and
type name.

### 8.2 Pinned Identity Across a Rename

```promise
type CreateUserRequest `serializable `id("a3f1b27c4d8e91035e2b8c7d4f1a09e60") {
    string display_name `id("0c8e5a2b9f4c771b3a8d6e0291c5f4b7")
        `key("name")               // wire still says "name"
        `doc("The user's full name.");

    // ... other fields unchanged ...
}
```

Source-side: `display_name` is the new identifier. Wire-side: the JSON key stays
`"name"` (via `` `key ``) and the persistence/MCP identity stays the same 32-hex
string (via `` `id ``). No data migration needed.

### 8.3 Function Schema

```promise
use schema;
use io;

read_text!(string path `doc("Absolute or relative file path.")) string
    `public `doc("Read a UTF-8 file from disk.") {
    return io.File.read!(path);
}

main!() {
    schema.Type fn = schema.for_func[read_text]();

    match fn {
        schema.Type.Function(_, _, name, desc, params, ret, failable) => {
            print_line("{name}: {desc ?: \"\"}");
            print_line("  failable: {failable}");
            for p in params {
                print_line("  {p.name}: {p.description ?: \"\"}");
            }
        },
        _ => {},   // for_func always returns a Function variant
    }

    print_line(fn.to_tool_input_schema());
}
```

---

## 9. Limitations and Boundaries

- **No runtime reflection.** Schemas are built at compile time. `schema.of(some_value)`
  taking a runtime value does not exist — the type must be known statically.
- **No anonymous closures.** Per §3, function values lose names. Code that wants to
  expose a closure as a tool must wrap it in a named function declaration.
- **No cross-module private fields.** A `` `serializable `` type with `_`-prefixed
  fields excludes them from both encode/decode and the schema, regardless of caller.
- **Cycles are bounded.** Recursive types (`type Tree `` `serializable `` { Tree[]
  children; }`) emit `schema.Type.Reference(id)` after the first occurrence; the full
  descriptor lives once in `definitions`.
- **Identity is not a security boundary.** `Hash128` collisions are negligible at
  realistic scale, but the hash is not a cryptographic commitment — anyone with the
  source can compute it. Authentication of cloud writes is the cloud-persistence
  module's job, not the schema module's.

---

## 10. Related Docs

- `docs/serialization-plan.md` — `` `serializable `` annotation, encode/decode
  synthesis, the field metas (`` `key ``, `` `skip ``, `` `flatten ``,
  `` `include_none ``, `` `required ``) the schema honors.
- `docs/ai-platform.md` — consumes `schema.Type` for tool definitions, structured
  output, and MCP server registration.
- `docs/cloud-persistence.md` — consumes `schema.Type` and `Hash128` as the wire
  identity of types, fields, and references in durable storage.
- `docs/language-design.md` §8 — meta annotation system (`` `doc ``, `` `serializable ``,
  `` `id ``, etc.).
- `docs/creating-modules.md` — `promise.toml`, `[module]` and `[executable]` tables,
  catalog vs community vs project modules.
