# Changelog

Promise is released in **epochs** — each epoch is an atomic release of the
entire platform: the compiler, the standard library, the catalog modules, and
the pinned LLVM toolchain. Projects pin an epoch in `promise.toml`; the
`next` channel (`epoch-next`) carries pre-releases ahead of the next stable
epoch.

## epoch 2026.0

The first stable epoch of Promise — a statically-typed, ownership-aware
language that compiles to native binaries via LLVM, designed for AI-agent
efficiency (self-contained, explicit, one obvious way).

Highlights of what ships in this epoch:

- **Language**: types and enums with inheritance, generics with
  monomorphization, ownership and borrows (`~`/`&`), explicit error handling
  (`?^`/`?`/`?!`), lambdas and closures, pattern matching, value types, and
  fixed-size arrays.
- **Concurrency**: an M:N scheduler (goroutines, channels, `select`, tasks),
  `Ref[T]` for reference-counted shared ownership, and IO-syscall handoff.
- **Standard library**: primitives, strings, containers (`Vector`, `Map`,
  `Set`), iterators, formatting/parsing, math, time, and serialization —
  auto-imported via `use std as _`.
- **Catalog modules**: `io`, `json`, `path`, `math`, `strings`, `os`, `time`,
  `net`, `gzip` — imported explicitly with `use <name>;`.
- **Toolchain**: a single self-contained `promise` binary (build, run, exec,
  test, format), with debug and `--release` (LTO) build modes, targeting
  native platforms and WebAssembly (`wasm32-wasi`, `wasm32-web`).
- **Distribution**: thin and full binary variants, content-addressed
  dependency blobs, and epoch-versioned side-by-side installs.

Supported release targets: `linux-amd64`, `darwin-arm64`, `windows-amd64`.
