# Promise Code Style

Conventions for Promise source code (`.pr` files). These rules apply to standard library, catalog modules, examples, and tests.

## Field and getter naming

Promise types frequently expose internal state through getters. Use a consistent naming convention so callers don't have to type the type name repeatedly and can predict the API from the field name.

### Rules

1. **Prefix private fields with `_`.**
   The underscore marks the field as an implementation detail not intended for direct external access.

2. **Name the public getter with the field name *without* the underscore.**
   The getter is the public-facing name; callers should never see the underscore. The underscore on the field clearly signals that the field is internal and lookups should go through the getter.

3. **Tag construction-only fields with `` `final ``.**
   If a field is only meant to be set in the constructor or factory, mark it `` `final ``. This:
   - Prevents accidental later mutation.
   - Documents intent at the declaration site.
   - Enables future optimizations (the compiler can assume the value never changes).

4. **Do not prefix getters with the type name.**
   `response.status_code`, `response.response_headers`, `request.request_method` all force callers to repeat context they already have at the call site (`response.status`, `response.headers`, `request.method` are unambiguous and shorter). Keep getter names short and field-aligned.

### Example

Bad — getter prefixed with type name, field name not aligned with getter, no `` `final ``:

```promise
type Response `public {
  int status;
  map[string, string] headers;
  string _body;

  get status_code int `public => this.status;
  get response_headers map[string, string] `public => this.headers;
  get body string `public => this._body;
}
```

Good — private fields underscored, getters match field name, immutable fields marked `` `final ``:

```promise
type Response `public {
  int _status `final;
  map[string, string] _headers `final;
  string _body `final;

  get status int `public => this._status;
  get headers map[string, string] `public => this._headers;
  get body string `public => this._body;
}
```

Callers read naturally:

```promise
Response r = http_get(url)?^;
println(r.status.to_string());
for k, v in r.headers { ... }
println(r.body);
```

### When the field is already public

If the field itself is intended to be part of the public API and there's no derived/transformed accessor, expose it directly without a getter — don't introduce a `_field` + getter pair purely for symmetry. Adding a getter is what justifies the underscore on the field.

## Comments

- **No decorative banner/separator comments.** Lines like `// ── Section ─────` provide no semantic value, consume tokens, and frequently contain non-ASCII characters that corrupt over time. If a section needs documentation, attach a `` `doc `` annotation to the relevant declaration.
- **Default to no comments.** Names should carry the meaning. Only add a comment when the *why* is non-obvious — a hidden constraint, a workaround, a subtle invariant. Don't restate what the code says.

## Documentation annotations

- Always add `` `doc("...") `` on every `` `public `` declaration (types, methods, functions, getters). The `` `doc `` text is the API surface that AI agents and tooling rely on; it should describe behavior, not restate the signature.

## Naming

- Use full English words in public APIs. Approved abbreviations are listed in `docs/language-design.md` §9.3a — when an approved abbreviation exists (e.g. `dir`, `env`, `id`, `len`, `min`, `max`), prefer the abbreviation; otherwise use the full word (`print_line`, not `println`).
- Side-effect-free, parameterless public access is a getter (`get name T`), not a function (`name() T`). Use a function only when the operation has parameters or side effects.

## Construction

- Use factory methods on the type (e.g. `Response.ok(...)`, `Server.bind(...)`) rather than free functions for constructing instances. Factories can set `` `final `` fields and live alongside the type's other methods. (See [feedback memory](../README.md) — saved separately.)
