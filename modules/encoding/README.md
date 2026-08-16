# encoding

Binary-to-text encodings in pure Promise (RFC 4648).

## What's here

- `hex_encode(u8[]) string` — bytes → lowercase hexadecimal (base16,
  RFC 4648 §8). Two characters per byte; empty input encodes to `""`.
- `hex_decode!(string) u8[]` — hexadecimal → bytes. Accepts upper- and
  lower-case digits. Raises `EncodingError` on odd-length input or a
  non-hex character.
- `EncodingError` — raised on malformed input, carries `at_index` (the
  offset into the input string, or `-1` when the failure isn't tied to a
  position).

base64 and base64url land in
[T1569](https://tracker/T1569) and will reuse `EncodingError`.

## Usage

```promise
use encoding;

u8[] data = u8[]();
data.push(0xDEu8);
data.push(0xADu8);
print_line(encoding.hex_encode(data));       // "dead"

u8[] back = encoding.hex_decode("dead")?!;
```
