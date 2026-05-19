# gzip

RFC 1952 / RFC 1951 gzip + DEFLATE support.

## Status

**Decompression only.** This module currently exposes just the decompression
path — enough to inflate gzip streams produced by other tools, which is what
compressed module embedding needs.

Full read/write support — gzip compression and `Reader`/`Writer` structural
adapters that compose with files and streams — is planned under
[T0334](https://tracker/T0334). Until that lands, do not use this module to
*produce* gzip streams.

## What's here today

- `gunzip!(u8[] &) u8[]` — gzip stream → raw bytes (header + trailer
  validated, CRC32 and ISIZE verified). Decodes only the first member of
  multi-member streams.
- `inflate!(u8[]) u8[]` — raw DEFLATE stream → bytes (RFC 1951)
- `crc32(u8[] &) u32` — IEEE polynomial CRC32 used by gzip and zlib
- `DecompressError` — raised on malformed input, carries a byte offset

## Design notes

- Pure Promise. No `native` shims, no fallback to a host gzip library. T0334
  will continue this approach for compression as well.
- Errors are `DecompressError` with a byte offset, so a malformed stream
  pinpoints where parsing failed.
