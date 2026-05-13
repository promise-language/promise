# Promise `toml` Module — API Proposal

Tracker: T0026. Status: planned (not yet implemented).

## 1. Motivation

[TOML](https://toml.io) is the de-facto configuration format for modern toolchains:
Rust's `Cargo.toml`, Python's `pyproject.toml`, Go's `go.work`/linters, and Promise's own
`promise.toml` / `catalog.toml`. Promise already parses a hand-rolled subset of TOML inside
the compiler (`compiler/internal/config/...`), but user code has no way to read or write
TOML without shelling out or re-implementing it.

A `toml` catalog module gives user code — and, eventually, Promise build tooling itself —
a single well-tested TOML implementation that plugs directly into the existing
`Encoder`/`Decoder` infrastructure. Every `Encodable`/`Decodable` type gains TOML support
automatically, matching the ergonomics of the `json` module.

Analogues in other ecosystems:

| Language | Library |
|----------|---------|
| Rust     | `toml` crate (serde-backed)                 |
| Python   | `tomllib` (stdlib, read-only) + `tomli-w`   |
| Go       | `github.com/BurntSushi/toml`                |
| JS/TS    | `@iarna/toml`                               |

## 2. Design Principles

1. **Mirror `json`**: `TomlEncoder is Encoder`, `TomlDecoder is Decoder`, same
   `encode_string[T]` / `decode_string[T]` free functions, same `TomlValue` enum for
   dynamic data. Anything that works with `json` works with `toml` by swapping the type.
2. **TOML v1.0.0 compliant**: target the [TOML 1.0.0 spec](https://toml.io/en/v1.0.0).
   Everything required by the spec is supported; extensions are avoided.
3. **Readable output by default**: emit top-level keys first, then `[table]` sections,
   then `[[array_of_tables]]`. Inline tables and arrays are used only where structurally
   required (nested arrays of scalars, or when the caller explicitly asks).
4. **Catalog-module constraints**: depends only on `std`. No other catalog imports, no
   `[require]`, no `epoch`. No module-level state — a `TomlEncoder` / `TomlDecoder` is
   an explicit value the caller owns.
5. **No hidden effects**: parsing is pure (input → value or error); encoding is pure
   (value → string). File I/O is the caller's job via the `io` module.

## 3. Quick Start

```
use toml;

type Server `public {
  string host;
  int port;
  bool tls;
}

main()! {
  string text = "host = \"localhost\"\nport = 8080\ntls = true\n";
  Server s = toml.decode_string[Server](text)?;
  print_line("server: {s.host}:{s.port}");

  string out = toml.encode_string[Server](s)?;
  print_line(out);
}
```

## 4. Assumed Dependencies

From `std` (auto-imported):

- `Encoder`, `Decoder`, `Encodable`, `Decodable`, `DecodeError` (`std/encode.pr`)
- `Builder`, `Writer`, `Format` (`std/builder.pr`, `std/format.pr`)
- `Vector[T]`, `Map[string, V]`, `Set[T]` (`std/vector.pr`, `std/map.pr`, `std/set.pr`)
- `string`, `int`, `uint`, `f64`, `bool`, `char` + their `parse`/`format` (`std/*.pr`)
- `error` base type (`std/error.pr`)

No new `std` types are required. No PAL additions.

## 5. Full API Surface

### 5.1 Errors

```
type TomlError is error `public `doc("A TOML parse or encode error.") {
  int line   `doc("1-based line number where the error was detected (0 if unknown).");
  int column `doc("1-based column number (0 if unknown).");
}
```

Parse errors are surfaced as `TomlError` (structural error with location).
Decoder-contract violations raised while materialising a `Decodable` value surface
as `std.DecodeError` (identical to the `json` module's behaviour).

### 5.2 `TomlEncoder`

```
type TomlEncoder is Encoder `public `doc("Encodes values as TOML text. Satisfies the Encoder interface.") {
  new(~this, bool inline_tables = false, int indent = 0);

  to_string(this) string `doc("Returns the accumulated TOML string.");

  // Encoder interface — see std/encode.pr
  encode_bool!(~this, bool value);
  encode_int!(~this, int value);
  encode_uint!(~this, uint value);
  encode_f64!(~this, f64 value);
  encode_string!(~this, string value);
  encode_none!(~this);

  begin_object!(~this, int field_count);
  end_object!(~this);
  begin_array!(~this, int length);
  end_array!(~this);
  encode_key!(~this, string name);
}
```

Notes:

- `encode_none` on an object field omits the field entirely (TOML has no `null`);
  at the top-level or inside an array it raises `TomlError` (arrays are
  homogeneously-typed in TOML 1.0).
- Nested objects emit as `[table.path]` headers unless `inline_tables = true`.
- Arrays of tables emit as `[[array.path]]` headers when all elements are tables.

### 5.3 `TomlDecoder`

```
type TomlDecoder is Decoder `public `doc("Decodes TOML text. Satisfies the Decoder interface.") {
  new!(~this, string input);

  // Decoder interface — see std/encode.pr
  decode_bool!(~this) bool;
  decode_int!(~this) int;
  decode_uint!(~this) uint;
  decode_f64!(~this) f64;
  decode_string!(~this) string;
  decode_none!(~this) bool;

  begin_object!(~this);
  end_object!(~this);
  begin_array!(~this) int;
  end_array!(~this);

  next_key!(~this) string?;
  skip_value!(~this);
  has_next_element!(~this) bool;
}
```

The constructor parses the full input into an in-memory `TomlValue` tree; the
decoder then walks the tree. This mirrors `JsonDecoder` and keeps the `Decoder`
contract (random-ish object traversal) easy to implement.

### 5.4 Dynamic values — `TomlValue`

```
enum TomlValue `public `doc("A dynamically-typed TOML value.") {
  Bool(bool value),
  Int(int value),
  Float(f64 value),
  String(string value),
  Datetime(string value)   `doc("RFC 3339 date/time as string; parsing into a typed Instant is v2."),
  Array(TomlValue[] items),
  Table(map[string, TomlValue] entries),

  is_bool(this) bool;
  is_int(this) bool;
  is_float(this) bool;
  is_string(this) bool;
  is_datetime(this) bool;
  is_array(this) bool;
  is_table(this) bool;

  as_bool(this) bool?;
  as_int(this) int?;
  as_float(this) f64?;
  as_string(this) string?;
  as_array(this) TomlValue[]?;
  as_table(this) map[string, TomlValue]?;

  get(this, string key) TomlValue?
    `doc("For Table: returns value for key, or none. For non-Table: none.");
  at(this, int index) TomlValue?
    `doc("For Array: returns element, or none if OOB or non-Array.");

  encode!(Encoder ~e);
  format(this, Writer ~w);
}
```

### 5.5 Free functions

```
encode_string[T](T value) string! `public
  `doc("Encodes any Encodable value as TOML text. Raises TomlError on failure.");

decode_string[T](string input) T! `public
  `doc("Decodes TOML text into any Decodable value. Raises TomlError or DecodeError.");

parse_value(string input) TomlValue! `public
  `doc("Parses TOML text into a dynamic TomlValue tree.");
```

## 6. Usage Patterns

### 6.1 Round-trip a typed config

```
type Config `public {
  string name;
  int workers;
  string[] hosts;
}

main()! {
  Config c = Config(name: "web", workers: 4, hosts: ["a", "b"]);
  string text = toml.encode_string[Config](c)?;
  Config back = toml.decode_string[Config](text)?;
  assert(back.name == c.name);
}
```

### 6.2 Read an unknown document with `TomlValue`

```
main()! {
  string src = read_file("config.toml")?;
  TomlValue root = toml.parse_value(src)?;
  if TomlValue? servers = root.get("servers") {
    if TomlValue[]? list = servers.as_array() {
      for TomlValue s in list {
        print_line(s.get("host")?.as_string()?);
      }
    }
  }
}
```

### 6.3 Encode as a string with inline tables

```
main()! {
  TomlEncoder e = TomlEncoder(inline_tables: true);
  config.encode(e)?;
  print_line(e.to_string());
}
```

## 7. Comparison Table

| Operation        | Promise `toml`                       | Rust `toml`                 | Python `tomllib`+`tomli_w`     | Go `BurntSushi/toml`           |
|------------------|--------------------------------------|-----------------------------|--------------------------------|--------------------------------|
| Decode to type   | `toml.decode_string[T](s)`           | `toml::from_str::<T>(s)`    | n/a (dict only, then map)      | `toml.Decode(s, &t)`           |
| Encode from type | `toml.encode_string[T](t)`           | `toml::to_string(&t)`       | `tomli_w.dumps(t.__dict__)`    | `toml.Marshal(t)`              |
| Dynamic parse    | `toml.parse_value(s)` → `TomlValue`  | `toml::from_str::<Value>`   | `tomllib.loads(s)` → dict      | `toml.DecodeReader` → `Primitive` |
| Error type       | `TomlError{line, column}`            | `toml::de::Error`           | `tomllib.TOMLDecodeError`      | `toml.ParseError`              |
| Spec version     | TOML 1.0.0                           | TOML 1.0.0                  | TOML 1.0.0                     | TOML 1.0.0                     |

## 8. Implementation Notes

- **Pure Promise, no PAL**: all parsing and emission is string/byte manipulation.
- **Parser**: single-pass recursive-descent over a char cursor. Builds a `TomlValue`
  tree directly; does not produce a separate token stream.
- **Table merging**: `[a.b]` then `[a.c]` must merge under a single `a` table. Track
  which tables were "explicitly defined" vs "implicitly created" to reject duplicate
  `[a]` headers per the spec.
- **Array-of-tables**: `[[servers]]` headers append to a `TomlValue.Array` under the
  parent key; mixed scalar/table arrays are rejected.
- **Encoder emission order**: first emit scalar fields at the current table scope,
  then recurse into sub-tables so headers come after their parent's scalars (TOML's
  "first rule of tables").
- **Datetimes**: v1 keeps them as raw RFC 3339 strings in `TomlValue.Datetime` and
  rejects malformed input at parse time. Typed `Instant` mapping is deferred (see §9).
- **Thread safety**: `TomlEncoder`/`TomlDecoder` are not shared — each is owned by the
  caller (`~this` on construction and on all mutating methods). No global state.
- **Cleanup**: both types own only `std` containers, which drop automatically; no
  `close()` is needed (and therefore no `use` binding is required by callers).
- **WASM**: fully supported — no filesystem, no threading, no PAL calls. No
  `` `target `` annotations needed.

## 9. Future Extensions (explicitly deferred)

- **Typed datetimes** — map `TomlValue.Datetime` to/from `time.Instant` once
  `modules/time` exposes RFC 3339 parsing.
- **Streaming encoder** — current API buffers into a `Builder`. A `Writer`-backed
  encoder can layer on top with no API break.
- **Preserve-comments round-trip** — would require a lossless CST; deliberately out
  of scope for v1. An opt-in `TomlDocument` type could be added later without
  disturbing `TomlEncoder`/`TomlDecoder`.
- **Schema validation** — out of scope; belongs in a separate module if needed.

## 10. Review Checklist

- [ ] Every public identifier uses full English words / approved abbreviations
- [ ] `snake_case` for funcs/fields, `PascalCase` for types/variants
- [ ] No function overloading (default params only)
- [ ] All `` `public `` declarations have `` `doc ``
- [ ] `TomlError` is `is error` with an `int code`-style surface (here `line`/`column`)
- [ ] Error-raising functions marked `!`
- [ ] No `` `target `` needed (pure Promise, WASM-safe)
- [ ] Quick-start compiles mentally against the spec
