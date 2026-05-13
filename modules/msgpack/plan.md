# Promise `msgpack` Module — API Proposal

Tracker: T0028. Status: planned (not yet implemented).

## 1. Motivation

[MessagePack](https://msgpack.org) is a compact, self-describing binary format —
"JSON shaped, but smaller and faster". It is the default wire format for Redis
scripting, Fluentd, many RPC frameworks (e.g. msgpack-rpc), and a common
alternative to JSON/Protobuf when schemas aren't pre-negotiated but speed and
size matter.

Promise already has a JSON module. Adding `msgpack` gives programs a binary,
length-prefixed, schema-optional format with the same `Encoder`/`Decoder`
ergonomics: every `Encodable`/`Decodable` type becomes MessagePack-ready for
free. Target use-cases: caches, IPC, structured logs, embedded/low-bandwidth
links, multi-language services where JSON is too verbose.

Analogues:

| Language | Library                            |
|----------|------------------------------------|
| Rust     | `rmp` / `rmp-serde`                |
| Python   | `msgpack-python`                   |
| Go       | `github.com/vmihailenco/msgpack/v5`|
| JS/TS    | `@msgpack/msgpack`                 |
| C++      | `msgpack-c`                        |

## 2. Design Principles

1. **Mirror `json` / `toml` / `yaml`**: `MsgpackEncoder is Encoder`,
   `MsgpackDecoder is Decoder`, same `encode_bytes[T]` / `decode_bytes[T]` /
   `parse_value` surface. Switching formats means swapping a type.
2. **Binary in, binary out**: the natural unit is `u8[]`, not `string`. Public
   free functions use `encode_bytes[T]` / `decode_bytes[T]` (vs. `encode_string`
   elsewhere) to make the byte-oriented API explicit and type-safe.
3. **MessagePack spec-compliant**: support the full
   [current spec](https://github.com/msgpack/msgpack/blob/master/spec.md) —
   nil, bool, int (fixed/uint/int 8–64), float32/64, str (fixstr/str8/16/32),
   bin (bin8/16/32), array (fixarray/array16/32), map (fixmap/map16/32), ext
   (fixext/ext8/16/32), timestamp (via ext type -1). Deprecated raw types are
   decoded for reading only.
4. **Length-prefixed throughout**: the spec gives us up-front counts for every
   container, which matches the `Encoder`/`Decoder` interface's
   `begin_object(field_count)` / `begin_array(length)` perfectly — no streaming
   concessions needed.
5. **Catalog-module constraints**: depends only on `std`. No PAL calls. No
   module-level state. Fully WASM-compatible.

## 3. Quick Start

```
use msgpack;

type Point `serializable {
  int x;
  int y;
}

main()! {
  Point p = Point(x: 3, y: 4);
  u8[] bytes = msgpack.encode_bytes[Point](p)?;
  print_line("{bytes.len} bytes");

  Point back = msgpack.decode_bytes[Point](bytes)?;
  assert(back.x == p.x && back.y == p.y);
}
```

## 4. Assumed Dependencies

From `std` (auto-imported):

- `Encoder`, `Decoder`, `Encodable`, `Decodable`, `DecodeError`
- `Vector[T]`, `Map[string, V]`
- `string`, `int`, `uint`, `f64`, `f32`, `bool`, `u8`
- `error` base type

No new `std` types. No PAL additions. **Not** required but noted: we do not
currently expose a bit-level byte-writer in `std`; the encoder builds directly
into a `Vector[u8]` using `push()` and a few `_native` big-endian helpers
defined inside the module.

## 5. Full API Surface

### 5.1 Errors

```
type MsgpackError is error `public `doc("A MessagePack parse or encode error.") {
  int offset `doc("Byte offset at which the error was detected (0 if unknown).");
}
```

Parse/structural errors raise `MsgpackError` with the byte offset. Decoder-
contract violations while materialising a `Decodable` value surface as
`std.DecodeError` (identical to `json`/`toml`/`yaml`).

### 5.2 `MsgpackEncoder`

```
type MsgpackEncoder is Encoder `public `doc("Encodes values as MessagePack bytes. Satisfies the Encoder interface.") {
  new(~this);

  to_bytes(~this) u8[] `doc("Returns the accumulated bytes and consumes the encoder.");

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

  // Msgpack-native extensions (optional — advanced callers)
  encode_f32!(~this, f32 value) `doc("Emits float32 (0xca). Saves 4 bytes vs encode_f64.");
  encode_binary!(~this, u8[] data) `doc("Emits bin8/16/32 for raw byte payloads.");
  encode_ext!(~this, i8 type_code, u8[] data) `doc("Emits fixext/ext8/16/32 with a user type code.");
}
```

Notes:

- Integer width is picked automatically to produce the shortest valid encoding:
  positive fixint, negative fixint, uint8/16/32/64, int8/16/32/64.
- String encoding picks fixstr/str8/16/32 by length (UTF-8 bytes).
- Map keys are required to be strings (per the `Encoder` interface). Msgpack
  natively allows arbitrary-typed keys, but the structural `Encoder` contract
  normalises to string keys for format portability.
- `begin_object(field_count)` and `begin_array(length)` require a pre-known
  count (map16/array16 or map32/array32 is chosen by size). Callers that don't
  know the count must buffer first — this matches the `Encoder` contract used
  by `json`/`toml`/`yaml`.

### 5.3 `MsgpackDecoder`

```
type MsgpackDecoder is Decoder `public `doc("Decodes MessagePack bytes. Satisfies the Decoder interface.") {
  new!(~this, u8[] input);

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

  // Msgpack-native extensions
  decode_f32!(~this) f32 `doc("Decodes a float32 (0xca). Errors on other types.");
  decode_binary!(~this) u8[] `doc("Decodes bin8/16/32.");
  peek_ext!(~this) (i8, u8[])? `doc("If the next value is an ext type, returns (type_code, data). Otherwise none.");
}
```

Unlike the `json`/`toml`/`yaml` decoders (which build an in-memory tree up
front), `MsgpackDecoder` walks the byte buffer directly — MessagePack's
length-prefixed framing makes streaming decode trivial and avoids an extra
allocation pass.

### 5.4 Dynamic values — `MsgpackValue`

```
enum MsgpackValue `public `doc("A dynamically-typed MessagePack value.") {
  Nil,
  Bool(bool value),
  Int(int value),
  Uint(uint value),
  Float32(f32 value),
  Float64(f64 value),
  String(string value),
  Binary(u8[] data),
  Array(MsgpackValue[] items),
  Map(map[string, MsgpackValue] entries)
    `doc("Keys are restricted to string — non-string keys raise MsgpackError."),
  Ext(i8 type_code, u8[] data),
  Timestamp(int seconds, uint nanos)
    `doc("Decoded from ext type -1. Encoded with appropriate fixext4/ext8 form."),

  is_nil(this) bool;
  is_bool(this) bool;
  is_int(this) bool;
  is_uint(this) bool;
  is_float(this) bool;
  is_string(this) bool;
  is_binary(this) bool;
  is_array(this) bool;
  is_map(this) bool;
  is_ext(this) bool;
  is_timestamp(this) bool;

  as_bool(this) bool?;
  as_int(this) int?;
  as_uint(this) uint?;
  as_f64(this) f64?;
  as_string(this) string?;
  as_binary(this) u8[]?;
  as_array(this) MsgpackValue[]?;
  as_map(this) map[string, MsgpackValue]?;

  get(this, string key) MsgpackValue?
    `doc("For Map: returns value for key. For non-Map: none.");
  at(this, int index) MsgpackValue?
    `doc("For Array: returns element, or none if OOB or non-Array.");

  encode!(Encoder ~e);
}
```

### 5.5 Free functions

```
encode_bytes[T](T value) u8[]! `public
  `doc("Encodes any Encodable value as MessagePack bytes. Raises MsgpackError on failure.");

decode_bytes[T](u8[] input) T! `public
  `doc("Decodes MessagePack bytes into any Decodable value.");

parse_value(u8[] input) MsgpackValue! `public
  `doc("Parses MessagePack bytes into a dynamic MsgpackValue tree.");
```

## 6. Usage Patterns

### 6.1 Typed round-trip

```
type Measurement `serializable {
  string sensor;
  f64 value;
  int timestamp;
}

main()! {
  Measurement m = Measurement(sensor: "t1", value: 21.5, timestamp: 1712000000);
  u8[] bytes = msgpack.encode_bytes[Measurement](m)?;
  Measurement back = msgpack.decode_bytes[Measurement](bytes)?;
  assert(back.sensor == m.sensor);
}
```

### 6.2 Inspecting an unknown payload

```
main()! {
  u8[] bytes = read_socket()?;
  MsgpackValue v = msgpack.parse_value(bytes)?;
  if MsgpackValue? cmd = v.get("cmd") {
    if string? name = cmd.as_string() {
      dispatch(name);
    }
  }
}
```

### 6.3 Custom ext type for UUIDs

```
main()! {
  MsgpackEncoder e = MsgpackEncoder();
  e.encode_ext(12, uuid_bytes)?;
  u8[] out = e.to_bytes();
}
```

## 7. Comparison Table

| Operation        | Promise `msgpack`                     | Rust `rmp-serde`              | Python `msgpack`              | Go `vmihailenco/msgpack`       |
|------------------|---------------------------------------|-------------------------------|-------------------------------|--------------------------------|
| Decode to type   | `msgpack.decode_bytes[T](b)`          | `rmp_serde::from_slice::<T>(&b)` | `msgpack.unpackb(b)` + map | `msgpack.Unmarshal(b, &t)`     |
| Encode from type | `msgpack.encode_bytes[T](t)`          | `rmp_serde::to_vec(&t)`       | `msgpack.packb(t.__dict__)`   | `msgpack.Marshal(t)`           |
| Dynamic parse    | `msgpack.parse_value(b)` → `MsgpackValue` | `rmp::from_slice` → `Value`   | `msgpack.unpackb(b)` → dict   | `msgpack.Unmarshal` → `interface{}` |
| Error type       | `MsgpackError{offset}`                | `rmp_serde::decode::Error`    | `msgpack.exceptions.UnpackException` | `msgpack.DecodeError`        |
| Spec             | current msgpack-spec (2021)           | current msgpack-spec          | current msgpack-spec          | current msgpack-spec           |

## 8. Implementation Notes

- **Pure Promise, no PAL**: all byte-level work is done via `Vector[u8]`
  `push`/`[]`, plus a handful of module-private helpers for big-endian u16/u32/
  u64/f32/f64 packing and unpacking.
- **Encoder strategy**: append directly into a `Vector[u8]` — no intermediate
  `Builder`. `to_bytes(~this)` moves the internal buffer out (no copy).
- **Decoder strategy**: position cursor over an `u8[]` input. Every read
  advances the cursor and bounds-checks against `input.len`; out-of-bounds
  reads raise `MsgpackError` with the current offset.
- **Integer width selection**: on encode, pick the narrowest encoding the value
  fits in (positive fixint, neg fixint, uint8/int8, uint16/int16, uint32/int32,
  uint64/int64). On decode, accept all widths and promote to `int` or `uint`
  as appropriate. Decode distinguishes `Int` (signed) from `Uint` (unsigned)
  only in the dynamic `MsgpackValue` — `decode_int!()` accepts both and raises
  `DecodeError` on overflow.
- **Strings vs binary**: MessagePack distinguishes `str` (UTF-8 text) from `bin`
  (raw bytes). Our `encode_string!` uses `str`; `encode_binary!` uses `bin`. On
  decode, `decode_string!` only accepts `str` — accepting `bin` would erase the
  distinction and invite mojibake bugs.
- **Timestamp**: the canonical msgpack timestamp format (ext type -1) has three
  variants — fixext4 (32-bit seconds), fixext8 (30-bit nanos + 34-bit seconds),
  ext8 (32-bit nanos + 64-bit seconds). The encoder picks the narrowest that
  fits; the decoder accepts all three.
- **Map key constraint**: msgpack allows any value as a map key. The structural
  `Encoder`/`Decoder` interface only supports string keys. Non-string keys
  encountered on decode raise `MsgpackError`. This is a deliberate trade-off
  for cross-format consistency.
- **Thread safety**: each encoder/decoder is caller-owned (`~this`); no globals.
- **Cleanup**: owns `Vector[u8]` / `Map[string, MsgpackValue]` / etc., all of
  which drop automatically; no `close()` needed.
- **WASM**: fully supported — no filesystem, no threading, no PAL. No
  `` `target `` annotations needed.

## 9. Future Extensions (explicitly deferred)

- **Non-string map keys** — would require widening the structural `Encoder`
  contract or a msgpack-specific side API (`encode_key_value!(MsgpackValue)`).
- **Streaming encoder without up-front length** — requires a two-pass or
  resizing-header strategy. Deferred; callers that need this today can buffer
  into an intermediate `MsgpackValue` tree and then encode.
- **Custom ext-type registry** — a type-level `is MsgpackExt` structural
  interface mapping user types to ext codes. Designed to layer on top without
  breaking the `Encoder`/`Decoder` contract.
- **Zero-copy decode** — returning `string` slices that reference the input
  buffer instead of copying. Requires ownership-aware slice types in `std`.

## 10. Review Checklist

- [ ] Every public identifier uses full English words / approved abbreviations
- [ ] `snake_case` for funcs/fields, `PascalCase` for types/variants
- [ ] No function overloading (default params only)
- [ ] All `` `public `` declarations have `` `doc ``
- [ ] `MsgpackError` is `is error` with an `offset` field for locations
- [ ] Error-raising functions marked `!`
- [ ] No `` `target `` needed (pure Promise, WASM-safe)
- [ ] Quick-start compiles mentally against the spec
- [ ] Encode width-selection (fixint / uint8 / uint16 / …) produces the
      shortest valid form
