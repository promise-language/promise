# Promise `yaml` Module — API Proposal

Tracker: T0027. Status: planned (not yet implemented).

## 1. Motivation

YAML is the lingua franca of declarative configuration and data-pipeline
description: Kubernetes manifests, GitHub Actions/GitLab CI, Ansible playbooks,
Docker Compose, Helm charts, dbt projects, OpenAPI/AsyncAPI specs. Any tool that
integrates with cloud-native or data-engineering ecosystems eventually needs to
read or write YAML.

Promise programs should be able to load a `deployment.yaml`, mutate a value, and
write it back — or emit structured logs as YAML — without shelling out or
re-implementing a parser. A `yaml` catalog module plugs directly into the
existing `Encoder`/`Decoder` infrastructure so every `Encodable`/`Decodable`
type gains YAML support automatically, mirroring the `json` and `toml` modules.

Analogues:

| Language | Library                         |
|----------|---------------------------------|
| Rust     | `serde_yaml` / `serde_yml`      |
| Python   | `PyYAML`, `ruamel.yaml`         |
| Go       | `gopkg.in/yaml.v3`              |
| JS/TS    | `js-yaml`                       |
| Java     | `SnakeYAML`                     |

## 2. Design Principles

1. **Mirror `json` / `toml`**: `YamlEncoder is Encoder`, `YamlDecoder is Decoder`,
   same `encode_string[T]` / `decode_string[T]` / `parse_value` surface, same
   dynamic-value enum (`YamlValue`). A caller switches formats by swapping one
   type.
2. **YAML 1.2 subset, JSON-compatible core**: target the intersection that is
   safe, unambiguous, and matches the `Encoder`/`Decoder` contract. Specifically
   supported: block & flow mappings, block & flow sequences, scalars with core
   schema tags (`str`, `int`, `float`, `bool`, `null`), multi-line strings
   (`|`, `>`), comments (parsed and discarded). **Not** supported in v1: tags
   (`!!binary`, custom `!Tag`), anchors & aliases (`&`, `*`), multi-document
   streams (`---`/`...` boundaries), merge keys (`<<`).
3. **Safe by default**: no code execution, no arbitrary-type construction — the
   `Encoder`/`Decoder` contract already prevents this, but we also reject the
   YAML 1.1 "Norway problem" truthy/falsy set and stick to YAML 1.2 core schema
   (`true`/`false`/`null` only, not `yes`/`no`/`on`/`off`).
4. **Readable output**: block style for maps and sequences by default; flow
   style only when the value is small and scalar. Two-space indent, no trailing
   spaces, newline-terminated.
5. **Catalog-module constraints**: depends only on `std`. No PAL calls. No
   module-level state. Fully WASM-compatible.

## 3. Quick Start

```
use yaml;

type Service `serializable {
  string name;
  int replicas;
  string[] ports;
}

main()! {
  string text =
    "name: web\n" +
    "replicas: 3\n" +
    "ports:\n" +
    "  - \"8080\"\n" +
    "  - \"8443\"\n";
  Service s = yaml.decode_string[Service](text)?;
  print_line("{s.name} x{s.replicas}");

  string out = yaml.encode_string[Service](s)?;
  print_line(out);
}
```

## 4. Assumed Dependencies

From `std` (auto-imported):

- `Encoder`, `Decoder`, `Encodable`, `Decodable`, `DecodeError`
- `Builder`, `Writer`, `Format`
- `Vector[T]`, `Map[string, V]`
- `string`, `int`, `uint`, `f64`, `bool`, `char` + `parse`/`format`
- `error` base type

No new `std` types. No PAL additions.

## 5. Full API Surface

### 5.1 Errors

```
type YamlError is error `public `doc("A YAML parse or encode error.") {
  int line   `doc("1-based line number (0 if unknown).");
  int column `doc("1-based column number (0 if unknown).");
}
```

Parse/structural errors surface as `YamlError`. Decoder-contract violations
while materialising an `Decodable` value surface as `std.DecodeError` (identical
to `json`/`toml`).

### 5.2 `YamlEncoder`

```
type YamlEncoder is Encoder `public `doc("Encodes values as YAML text. Satisfies the Encoder interface.") {
  new(~this, int indent = 2, bool flow = false);

  to_string(this) string `doc("Returns the accumulated YAML string.");

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

- `encode_none` emits the bare `null` scalar. In an object field position the
  field is still written with a `null` value (YAML, unlike TOML, has a null).
- Block vs flow style is picked automatically for readability unless `flow` is
  true (force everything to `{ ... }` / `[ ... ]`).
- Strings are emitted quoted when necessary to disambiguate from bool/int/float/
  null, or when they contain control characters. Plain scalars otherwise.

### 5.3 `YamlDecoder`

```
type YamlDecoder is Decoder `public `doc("Decodes YAML text. Satisfies the Decoder interface.") {
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

The constructor parses the full input into an in-memory `YamlValue` tree; the
decoder walks the tree. Same strategy as `JsonDecoder`/`TomlDecoder`.

### 5.4 Dynamic values — `YamlValue`

```
enum YamlValue `public `doc("A dynamically-typed YAML scalar, sequence, or mapping.") {
  Null,
  Bool(bool value),
  Int(int value),
  Float(f64 value),
  String(string value),
  Sequence(YamlValue[] items),
  Mapping(map[string, YamlValue] entries),

  is_null(this) bool;
  is_bool(this) bool;
  is_int(this) bool;
  is_float(this) bool;
  is_string(this) bool;
  is_sequence(this) bool;
  is_mapping(this) bool;

  as_bool(this) bool?;
  as_int(this) int?;
  as_float(this) f64?;
  as_string(this) string?;
  as_sequence(this) YamlValue[]?;
  as_mapping(this) map[string, YamlValue]?;

  get(this, string key) YamlValue?
    `doc("For Mapping: returns value for key. For non-Mapping: none.");
  at(this, int index) YamlValue?
    `doc("For Sequence: returns element, or none if OOB or non-Sequence.");

  encode!(Encoder ~e);
  format(this, Writer ~w);
}
```

### 5.5 Free functions

```
encode_string[T](T value) string! `public
  `doc("Encodes any Encodable value as YAML text. Raises YamlError on failure.");

decode_string[T](string input) T! `public
  `doc("Decodes YAML text into any Decodable value.");

parse_value(string input) YamlValue! `public
  `doc("Parses YAML text into a dynamic YamlValue tree.");
```

## 6. Usage Patterns

### 6.1 Typed round-trip

```
type Pod `serializable {
  string name;
  string[] containers;
}

main()! {
  Pod p = Pod(name: "web", containers: ["nginx", "sidecar"]);
  string text = yaml.encode_string[Pod](p)?;
  Pod back = yaml.decode_string[Pod](text)?;
  assert(back.containers.len == 2);
}
```

### 6.2 Inspecting an unknown document

```
main()! {
  string src = read_file("deployment.yaml")?;
  YamlValue root = yaml.parse_value(src)?;
  if YamlValue? spec = root.get("spec") {
    if YamlValue? replicas = spec.get("replicas") {
      print_line("replicas = {replicas.as_int()?}");
    }
  }
}
```

### 6.3 Emit logs as YAML

```
type Event `serializable {
  string level;
  string message;
  int timestamp;
}

main()! {
  Event e = Event(level: "info", message: "started", timestamp: 1712000000);
  print_line(yaml.encode_string[Event](e)?);
}
```

## 7. Comparison Table

| Operation        | Promise `yaml`                       | Rust `serde_yaml`              | Python `PyYAML` (safe)            | Go `yaml.v3`               |
|------------------|--------------------------------------|--------------------------------|-----------------------------------|----------------------------|
| Decode to type   | `yaml.decode_string[T](s)`           | `serde_yaml::from_str::<T>(s)` | `yaml.safe_load(s)` → dict → map  | `yaml.Unmarshal([]byte(s), &t)` |
| Encode from type | `yaml.encode_string[T](t)`           | `serde_yaml::to_string(&t)`    | `yaml.safe_dump(t.__dict__)`      | `yaml.Marshal(t)`          |
| Dynamic parse    | `yaml.parse_value(s)` → `YamlValue`  | `serde_yaml::from_str::<Value>`| `yaml.safe_load(s)`               | `yaml.Unmarshal` → `map[string]any` |
| Error type       | `YamlError{line, column}`            | `serde_yaml::Error`            | `yaml.YAMLError`                  | `yaml.TypeError`           |
| Spec             | YAML 1.2 core schema subset          | YAML 1.2                       | YAML 1.1 + 1.2 safe subset        | YAML 1.2                   |

## 8. Implementation Notes

- **Pure Promise, no PAL**: all parsing and emission is string/byte manipulation.
- **Parser**: indentation-aware recursive-descent. A preprocessing pass tokenises
  a line stream (tracking indent level, line number, column) and folds block
  scalars (`|` / `>`) into flat strings before the structural parser runs. This
  keeps the structural parser orthogonal to whitespace handling.
- **Scalar resolution**: follow the YAML 1.2 core schema resolution rules in
  order: `null` (empty/`~`/`null`), `bool` (`true`/`false` only — no `yes`/`on`),
  `int` (`[+-]?[0-9]+` in dec, `0x`, `0o`), `float` (incl. `.inf`, `.nan`),
  else `string`. No YAML 1.1 fuzzy booleans.
- **Quoting**: on emit, quote a string if it (a) starts with a character that
  would make it look like a flow indicator, tag, anchor, alias, or block
  scalar; (b) matches the scalar-resolution rules for bool/int/float/null; or
  (c) contains newlines or control chars (double-quote style with `\n`, `\t`,
  `\"`, `\\` escapes).
- **Key ordering**: insertion order from the `Encoder` is preserved; YAML does
  not require sorted keys and round-trip fidelity is more useful than
  canonicalisation.
- **Multi-document streams** (`---`/`...`): rejected in v1 — parse_value errors
  on multiple documents. A `parse_values(string) YamlValue[]` is sketched in §9.
- **Anchors/aliases** (`&x` / `*x`): rejected in v1 — would require a resolve
  pass and reference-tracking in `YamlValue`. Deferred.
- **Thread safety**: each encoder/decoder is caller-owned (`~this`); no globals.
- **Cleanup**: owns only `std` containers, which drop automatically; no
  `close()` needed.
- **WASM**: fully supported — no filesystem, no threading, no PAL. No
  `` `target `` annotations needed.

## 9. Future Extensions (explicitly deferred)

- **Multi-document streams** — `parse_values(string) YamlValue[]!` and a
  corresponding `encode_values`. API shape chosen to avoid breaking the existing
  single-document functions.
- **Anchors & aliases** — structural additions to `YamlValue` (`Anchor(string, YamlValue)`,
  `Alias(string)`) plus a resolve step.
- **Tags** — `!!binary`, `!!timestamp`, and user tags require a tag-aware variant
  in `YamlValue` (`Tagged(string tag, YamlValue inner)`).
- **Comment preservation** — requires a lossless CST; deliberately out of scope
  for v1.
- **Streaming encoder** — current API buffers into a `Builder`. A `Writer`-backed
  encoder can layer on top with no API break.

## 10. Review Checklist

- [ ] Every public identifier uses full English words / approved abbreviations
- [ ] `snake_case` for funcs/fields, `PascalCase` for types/variants
- [ ] No function overloading (default params only)
- [ ] All `` `public `` declarations have `` `doc ``
- [ ] `YamlError` is `is error` with `line`/`column` location
- [ ] Error-raising functions marked `!`
- [ ] No `` `target `` needed (pure Promise, WASM-safe)
- [ ] Quick-start compiles mentally against the spec
- [ ] Unsupported-in-v1 features (anchors, tags, multi-doc) produce clear
      `YamlError` messages at parse time
