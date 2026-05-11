# Build Tools

This document describes the Promise compiler's build tooling system. All build tools live under `tools/` as a single Go module, compiled to `bin/` via a meta-builder.

## Bootstrap

```bash
./make        # compile all tools to bin/
bin/build                       # build the compiler
bin/verify --wasm               # full pre-commit check
```

The only prerequisite is Go 1.25+. Running `./make` compiles all tool binaries into `bin/`. Each binary embeds a hash of the `tools/` source files and refuses to run if the source has changed, prompting you to re-run `./make`.

## Tool Inventory

| Binary | Purpose |
|--------|---------|
| `bin/build` | Build the compiler binary (`bin/promise`). Handles ANTLR parser generation, resource embedding, LLVM detection, and Go compilation. |
| `bin/verify` | Pre-commit verification: format, vet, build, and test. Supports `--shared`, `--wasm`, `--clean`. |
| `bin/test` | Run test suites. Modes: `go`, `promise`, `all`. Supports `--wasm`, `--clean`. |
| `bin/format` | Format Go code (`gofmt`) and Promise code (`promise format`). |
| `bin/vet` | Run `go vet` on compiler packages (excluding generated parser). |
| `bin/coverage` | Test coverage analysis for Go packages and Promise tests. |
| `bin/stress` | Stress testing for flaky test detection. |
| `bin/setup` | One-time dev setup (git hooks). |
| `bin/prereqs` | Install build prerequisites (LLVM, Go, Java, wasmtime). |

## Architecture

All tools share a common library (`tools/build/common/`) and are thin `main.go` wrappers under `tools/build/cmd/`. When one tool needs another (e.g., verify calls build), it calls the common library directly — no subprocess spawning.

```
tools/build/
├── go.mod              # single module
├── common/             # shared implementation
│   ├── root.go         # repo root detection
│   ├── hash.go         # source hash computation
│   ├── stale.go        # staleness check
│   ├── platform.go     # OS/arch, LLVM tool discovery
│   ├── exec.go         # subprocess helpers
│   ├── build.go        # build pipeline
│   ├── verify.go       # verify pipeline
│   └── ...
└── cmd/
    ├── make/main.go    # meta-builder (compiles all tools)
    ├── build/main.go
    ├── verify/main.go
    └── ...
```

## Build Pipeline (`bin/build`)

The build tool performs the following steps:

### 1. Git hooks setup

Configures git to use `.githooks/` for hooks:
```
git config core.hooksPath .githooks
```

### 2. ANTLR parser generation

Downloads the ANTLR 4.13.1 JAR (if not cached) and generates the Go lexer/parser from the grammar files:
- Input: `compiler/grammar/PromiseLexer.g4`, `compiler/grammar/PromiseParser.g4`
- Output: `compiler/internal/parser/` (Go source files)
- Requires: Java 11+

Generation is skipped if the output is up to date.

### 3. Resource embedding

Copies project files into `compiler/cmd/promise/resources/` for Go's `embed` directive:

| Source | Destination |
|--------|-------------|
| `catalog.toml` | `resources/catalog.toml` |
| `docs/language-guide.md` | `resources/language-guide.md` |
| `examples/` | `resources/examples/` |
| `modules/` | `resources/modules/` |
| `modules/std/*.pr` | `compiler/internal/testutil/testdata/std/` |

Also computes `resources/.sources.sha256` — a sorted list of SHA256 hashes for all module and catalog files. This hash is used for build cache invalidation.

**Linux only:** Copies musl C runtime objects (`crt1.o`, `crti.o`, `crtn.o`, `libc.a`) from `/usr/lib/x86_64-linux-musl/` into `resources/crt/x86_64-linux-musl/` for static linking.

### 4. LLVM detection

The compiler requires LLVM 22-25. The build tool searches for `opt` and `lld` in this order:

**macOS (Homebrew):**
1. Versioned: `/opt/homebrew/opt/llvm@25` down to `@22`, then `/usr/local/opt/llvm@25` down to `@22`
2. Unversioned: `/opt/homebrew/opt/llvm`, `/usr/local/opt/llvm`
3. Versioned in PATH: `opt-25` down to `opt-22`
4. Unversioned in PATH: `opt`

**Linux:**
1. Versioned in PATH: `opt-25` down to `opt-22`
2. Unversioned in PATH: `opt` (version checked)

**Windows:**
1. `C:\Program Files\LLVM\bin`
2. `%USERPROFILE%\LLVM\bin`

The linker (`lld`) follows the same search pattern. On macOS it looks for `ld64.lld`, on Linux `ld.lld`, on Windows `lld-link`.

### 5. Go compilation

Builds the compiler with version information:
```
go build -buildvcs=false -ldflags "-X main.version=<version>" -o bin/promise ./cmd/promise
```

The version string format:
- Dev build: `<epoch>-<gitsha7>` (e.g., `9-a1b2c3d`)
- Release build: `<epoch>` only

The epoch is read from `catalog.toml`. After building, writes `bin/.promise.hash` (SHA256 of the binary) for cache invalidation.

### 6. Release builds (`bin/build --release`)

Release builds embed LLVM tools in the binary for self-contained distribution:

**Linux:** Gzips `opt`, `llc`, `lld`, and `libLLVM.so` from the system LLVM installation.

**macOS:** Gzips `opt`, `llc`, `lld`, `libLLVM.dylib`, and all transitive dylib dependencies (discovered via `otool -L`). At runtime, extracted Mach-O binaries are patched with `install_name_tool` and re-signed with `codesign`.

Release builds compile with `-tags embed_llvm` to enable the embedded tool extraction code paths.

## Verify Pipeline (`bin/verify`)

The verify tool orchestrates the full pre-commit check:

1. **Format** — `gofmt -w .` in compiler/, then `promise format` on all `.pr` files
2. **Build** — full build pipeline (see above)
3. **Vet** — `go vet ./...` excluding `internal/parser` (auto-generated)
4. **Go tests** — `go test ./...` in compiler/
5. **Promise tests (host)** — `promise test tests/... modules/... examples/...`
6. **Promise tests (WASM)** — if `--wasm` flag, same with `-target wasm32-wasi`

### Flags

- `--local` — use `.promise-home/` in the repo instead of `~/.promise` (default; avoids polluting user home)
- `--shared` — use `~/.promise` shared cache instead of the local `.promise-home/`
- `--wasm` — include wasm32-wasi target tests (requires `wasmtime`)
- `--clean` — clear Go and Promise test caches before running

### Global lock

Concurrent verify runs from different worktrees are serialized via a file lock (`~/.promise/verify.lock`), preventing resource contention.

## Staleness Check

The meta-builder (`./make`) computes an FNV hash of all `.go` and `go.mod` files under `tools/` and injects it into each binary at compile time via `-X main.sourceHash=<hash>`. When a tool binary runs, it recomputes the hash and compares. If they differ, the binary prints:

```
tools source has changed — run: ./make
```

and exits with code 1. This ensures you never accidentally use a stale tool after editing the build system.

## Previous Build System

This Go-based tool system replaced an earlier collection of platform-specific scripts:

- `build` (bash) + `build.ps1` (PowerShell) — compiler build
- `compiler/Makefile` — ANTLR generation, resource embedding, LLVM bundling
- `bin/verify.sh` + `bin/verify.ps1` — pre-commit verification
- `bin/test.sh` — test runner
- `bin/format.sh` — code formatting
- `bin/coverage.sh` — coverage analysis
- `bin/stress.sh` — stress testing
- `bin/setup.sh` — git hooks
- `bin/install-prereqs.sh` + `bin/install-prereqs.ps1` — prerequisite installation
- `bin/_env.sh` — PATH setup for deterministic builds

The motivation for replacement was that changes on one platform (e.g., Linux) would silently break another (e.g., Windows) due to duplicated logic across bash/PowerShell/Make. A single Go codebase eliminates this drift by construction.
