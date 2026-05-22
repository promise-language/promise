# gzip

RFC 1952 / RFC 1951 gzip + DEFLATE in pure Promise.

## What's here

- `gunzip!(u8[] &) u8[]` — gzip stream → raw bytes (header + trailer
  validated, CRC32 and ISIZE verified). Decodes only the first member of
  multi-member streams.
- `gzip_encode(u8[] &) u8[]` — raw bytes → gzip stream. Output is valid
  gzip readable by standard tools (`gunzip(1)`, Python's `gzip` module,
  Go's `compress/gzip`, etc.). The function is named `gzip_encode` rather
  than `gzip()` to avoid conflicting with the module name on `use gzip;`.
- `inflate!(u8[]) u8[]` — raw DEFLATE stream → bytes (RFC 1951).
- `deflate(u8[] &) u8[]` — raw bytes → DEFLATE stream (RFC 1951).
- `crc32(u8[] &) u32` — IEEE polynomial CRC32 used by gzip and zlib.
- `GzipWriter` — buffered Writer adapter. Accumulates input through the
  standard `Writer` interface, then emits a gzip stream via
  `finish_to(Writer)` or `to_bytes()`.
- `GunzipReader` — buffered Reader adapter. Decompresses a complete gzip
  stream on construction, then serves bytes through the standard `Reader`
  interface.
- `gunzip_from!(Reader)` — reads a complete gzip stream from any Reader
  (file, in-memory reader, ...) and returns the decompressed bytes.
- `DecompressError` — raised on malformed input, carries a byte offset.

## Status

**Round-trip correct, output is valid gzip, but not yet size-reduced.** The
encoder currently emits stored (uncompressed) DEFLATE blocks only — every
block contains the raw input, just wrapped in valid DEFLATE/gzip framing.
That meets the contract (output decompresses correctly via standard tools)
without the complexity of a Huffman + LZ77 encoder. Real compression is
tracked as [T0462](https://tracker/T0462).

## Composition

`GzipWriter` accepts any `Writer` as its drain, and `GunzipReader` /
`gunzip_from` accept any `Reader` as their source. So gzip wraps cleanly
around files, in-memory builders, custom byte-array readers, etc.:

```promise
use gzip;
use io;

// Compress to a file
File f = File.create("data.gz")?!;
GzipWriter w = GzipWriter();
w.write_string("hello, world")?!;
w.finish_to(f)?!;
f.close()?!;

// Decompress from a file
File f2 = File.open("data.gz", readonly: true)?!;
u8[] bytes = gunzip_from(f2)?!;
f2.close()?!;
```

## Design notes

- Pure Promise. No `native` shims, no fallback to a host gzip library.
- `GzipWriter` and `GunzipReader` use buffered (not streaming) decode/encode.
  True streaming requires storing an underlying Writer/Reader as a field,
  which is currently blocked by [T0460](https://tracker/T0460) (structural
  fields don't drop correctly). Once that lands the API can evolve.
- Errors are `DecompressError` with a byte offset, so a malformed stream
  pinpoints where parsing failed.
