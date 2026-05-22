# schema — Type-Driven Schema Generation

**Status: proposed.** Tracker task: [T0446](../../README.md).

`modules/schema` provides a compile-time descriptor for any Promise declaration:
its fields, methods, generic arguments, optionality, defaults, and a stable 128-bit
content-addressed identity. The descriptor is the shared substrate for the AI
platform (`docs/ai-platform.md`) and cloud persistence
(`docs/cloud-persistence.md`).

## Quick start

```promise
use schema;

type CreateUserRequest `serializable
    `doc("Request to create a new user.") {
    string name `doc("The user's full name.");
    string email `doc("Email address.");
    int? age `doc("Optional age in years.");
    string role = "viewer";
}

main!() {
    schema.Type t = schema.of[CreateUserRequest]();
    print_line(t.to_json_schema());
}
```

## API surface (summary)

- `schema.Type` — tagged enum: `Object` / `Array` / `Map` / `Scalar` / `Enum` /
  `Function` / `Optional` / `Reference`.
- Helpers: `schema.Field`, `schema.Variant`, `schema.Param`, `schema.ScalarKind`.
- Identity: `schema.Hash128`, `schema.Origin` (`Embedded` / `External(url)` /
  `Project(project_id)`).
- Free functions: `schema.of[T]()`, `schema.for_func[F]()`.
- Renderers on `Type`: `to_json_schema()`, `to_openapi()`, `to_tool_input_schema()`.

## Compiler hooks (in scope of T0446)

- `` `id("<32-hex>") `` meta — pin a construct's `Hash128`.
- `` `entity `` meta — implies `` `serializable ``, synthesizes a hidden id field,
  switches encoding to id-only when the type appears as a field of another type.
- Schema-synthesis hook in `compiler/internal/sema/serialize.go` — emits a hidden
  `_schema_descriptor()` factory per `` `serializable `` type/enum.
- `[executable]` table support in `promise.toml` parsing with lazy id resolution.

## Full design

See [`docs/schema.md`](../../docs/schema.md) for design constraints, identity
composition rules, the `` `id `` and `` `entity `` meta specifications, project
identity in `promise.toml`, the compiler extension contract, and worked examples.

## Related modules

- `docs/ai-platform.md` — consumes `schema.Type` for tool definitions, structured
  output, and MCP server registration.
- `docs/cloud-persistence.md` — consumes `schema.Type` and `Hash128` as the wire
  identity of types, fields, and references in durable storage.
- `docs/serialization-plan.md` — the `` `serializable `` annotation and the field
  metas (`` `key ``, `` `skip ``, `` `flatten ``, `` `include_none ``,
  `` `required ``) that the schema descriptor honors.
