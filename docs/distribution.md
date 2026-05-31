# Distribution & Installation

> **Status (2026-05-30).** This document describes the **target** distribution architecture. Some of it is implemented today, some is planned; each section is marked. The headline change from the original model is twofold: (1) heavy dependencies (LLVM tools, wasm runner, CRTs, target sysroots) move from *embedded-in-the-binary* to a *content-addressed cache fetched on demand*, so the compiler ships in **thin** and **full** variants that behave identically; and (2) the on-`PATH` entry point becomes a **tiny stub** (written in Promise, extracted at install) instead of a full copy of the compiler. See §1 for the model, §7 / [release-automation.md](release-automation.md) for how releases are built and published.
>
> **Implemented today:** self-contained `--release` binary (embeds *everything* — LLVM, musl CRT, stdlib) on Linux (amd64) and macOS (arm64 + amd64); `promise install` with the epoch layout; epoch dispatch via *shim-in-binary*; `promise sync` to fetch epochs. **Planned:** thin/full split + content-addressed dependency store (§1, §4); the embedded Promise stub replacing shim-in-binary (§2.5); `promise update` self-update rename (§2.6); Windows zero-dependency install (§5.2). Windows compiler support itself is in progress (see [windows-support.md](windows-support.md)).

---

## 1. Design

Promise is distributed as **one downloadable artifact per platform** — the compiler binary. That single artifact is simultaneously:

- the **installer** (`promise install`),
- the source of the on-`PATH` **stub** (extracted at install time),
- the **compiler / tool / package manager**, and
- the **self-updater** (`promise update`).

These are *logically one thing* — you download one file — but the stub is *physically a separate, tiny program* carried inside the compiler and extracted during install (§2.5). This is the resolution of "same binary vs different binaries": ship one, extract the small piece.

### 1.1 What is always in the binary vs fetched on demand

The original model embedded **everything** in the binary (~61 MB on Linux, more on macOS). That does not scale: a single binary cannot carry every dependency for every *cross-compilation target*. The target model separates two classes of payload:

| Class | Examples | How it's carried |
|-------|----------|------------------|
| **Always embedded** (small, always needed) | Compiler frontend + codegen, standard library source, the tiny stub, the **dependency manifest** | Compiled in / `go:embed` |
| **Fetched on demand** (large, target-specific) | LLVM host tools (`opt`, `llc`, `lld`, `libLLVM`), wasm runner (`wasmtime`/Node harness), CRTs (musl), target sysroots | Content-addressed cache (§4) |

The binary embeds a **manifest** — a list of `(logical name, sha256, size, fetch coordinates)` for every heavy dependency it might need. At the moment a dependency is required, the compiler looks it up in the shared content-addressed cache (`~/.promise/cache/blobs/sha256/<hash>`); if present it is used, if absent it is fetched from the release, **verified against the embedded hash**, cached, and used. The hash in the binary is the **trust anchor** — a fetched blob needs no separate signature, only a hash match.

### 1.2 Thin and full variants

Because dependencies are addressed by hash rather than embedded, every release produces two variants per platform:

| Variant | Size (approx.) | Behavior |
|---------|---------------|----------|
| **thin** | ~20 MB | Embeds only the manifest. Fetches host toolchain blobs on first use. Needs the network once per (epoch, dependency). |
| **full** | ~150 MB | Same binary, but ships the host-workflow blobs pre-staged into the cache (or embedded and extracted at install). Needs no network for the host's default workflow. The archival / air-gapped / "must still work in 5 years" choice. |

Both behave identically — the only difference is the disk-vs-network tradeoff. `promise build hello.pr` on a thin binary fetches host LLVM the first time and is identical thereafter; the full binary never needs to.

**"Full" means full for the host's default workflow — not for every target.** You cannot embed every target's toolchain, so **cross-target dependencies are always fetched on demand**, even on a full binary. Targeting `wasm32-wasi` or `linux-arm64` from a macOS host fetches those target blobs on first use regardless of variant.

### 1.3 Why content-addressing (not per-epoch copies)

A content-addressed store **deduplicates across epochs and targets**. Two installed epochs built on the same LLVM 22 share **one** cached copy keyed by hash, instead of each epoch carrying its own `bin/llvm/`. Epoch directories shrink to *references* (which blob hashes they need); `promise remove <epoch>` becomes "drop references, GC unreferenced blobs." This supersedes the per-epoch `bin/llvm/` layout in [epoch-versioned-installs.md](epoch-versioned-installs.md) — see §4.

---

## 2. Installation

### 2.1 Linux & macOS — install script *(implemented; download semantics unchanged)*

```sh
curl -sSf https://promise-lang.org/install.sh | sh
```

The script ([scripts/install.sh](../scripts/install.sh)) detects OS/arch, downloads the matching binary (thin by default) from GitHub Releases, verifies its SHA256 against `SHA256SUMS`, and runs `./binary install` (§2.4). Pin an epoch or pick the full variant:

```sh
curl -sSf https://promise-lang.org/install.sh | sh -s -- --epoch 2026.0
curl -sSf https://promise-lang.org/install.sh | sh -s -- --full
```

### 2.2 Windows — install script *(planned)*

Windows needs **two one-liners**, one per shell, because the PowerShell idiom does not work in `cmd.exe` (`irm`/`iex` are PowerShell cmdlets — pasting them into `cmd.exe` yields `'irm' is not recognized`). This mirrors the Claude CLI installer:

```powershell
# PowerShell
irm https://promise-lang.org/install.ps1 | iex
```

```bat
:: cmd.exe (curl.exe ships with Windows 10 1803+)
curl -fsSL https://promise-lang.org/install.cmd -o install.cmd && install.cmd && del install.cmd
```

`install.ps1` is the real implementation (platform/arch detection, download, checksum, `promise install`, **User `PATH` via `[Environment]::SetEnvironmentVariable(..., 'User')`**). `install.cmd` is a **thin shim** that re-invokes PowerShell (`powershell -ExecutionPolicy Bypass -Command "irm … | iex"`) so there is a single real implementation. Direct download (§2.3) is the no-script fallback for locked-down environments.

**Zero local dependencies is the bar.** The Windows artifact must embed the Windows SDK / UCRT link surface (`.lib` stubs) the same way macOS will embed its SDK stubs (§5) — installing on a fresh Windows machine must "just work" with no "install Visual Studio Build Tools ≥ version X first." See §5.2.

### 2.3 Direct download *(implemented)*

```sh
# Linux amd64, thin
curl -LO https://github.com/promise-language/promise/releases/latest/download/promise-linux-amd64
chmod +x promise-linux-amd64
./promise-linux-amd64 install
```

`promise install` handles everything from here. Append `-full` to the asset name for the full variant.

### 2.4 What `promise install` does *(install flow updated for the stub + blob model)*

`promise install` (`runInstall()` in [cmd/promise/main.go](../compiler/cmd/promise/main.go)) installs into the Promise home directory (default `~/.promise/`, overridable via `PROMISE_HOME`):

1. **Determine the embedded epoch** from the embedded catalog.
2. **Install the compiler** → `~/.promise/epochs/<epoch>/bin/promise` (move/copy self).
3. **Stage dependencies**: a *full* binary unpacks its bundled blobs into the content-addressed cache (`~/.promise/cache/blobs/sha256/<hash>`); a *thin* binary records the manifest so they are fetched on first use.
4. **Extract the stub — forward-only** → `~/.promise/bin/promise`. The stub is replaced **only if the embedded stub's version is newer** than the installed one (§2.5). Stubs are never downgraded.
5. **Write `~/.promise/active`** with the current epoch.
6. **Set `PATH`**: print the export line (Unix) or set the User `PATH` env var (Windows).

Resulting layout:

```
~/.promise/
  bin/
    promise              ← the tiny stub (Promise-compiled, forward-updated)
  active                 ← "2026.0"
  epochs/
    2026.0/bin/promise   ← the real compiler for this epoch
  cache/
    blobs/sha256/<hash>  ← content-addressed dependency store (shared)
    build/               ← compile cache
```

### 2.5 The stub (launcher) *(planned — replaces shim-in-binary)*

The on-`PATH` `~/.promise/bin/promise` is the **stub**: the thing the user runs, which does *not* compile anything itself — it locates the correct epoch's real compiler and hands off to it. (Synonyms seen elsewhere: *shim*, *launcher*, *trampoline*; they all mean this one object.)

**Today** this is *shim-in-binary*: `~/.promise/bin/promise` is a full copy of the compiler that detects its stub role via a `.promise.shim` marker and re-execs the epoch binary ([shim.go](../compiler/cmd/promise/shim.go)). **The target** is a dedicated tiny stub. The difference is not only disk and cold-start — it is a real simplification and reliability win:

- **The compiler stops being a trampoline.** Delete `shimDispatch()`, the `PROMISE_NO_SHIM` recursion guard, the `.promise.shim` marker, and every "am I a stub or the compiler?" branch from the compiler. **You get what you run**: invoking the big binary directly always runs the compiler, full stop — no surprise hand-off. If the directly-invoked compiler's epoch differs from the project's pinned epoch, that is a **warning, not a silent trampoline** to something unexpected.
- **The stub `exec`-replaces itself** with the real compiler (Unix `execve`; Windows uses the closest available, see caveat). Same process, same PID. This eliminates a whole class of bugs: record-the-PID / run / kill-the-PID no longer leaves the real compiler alive as an orphan, and signals reach the compiler directly. (Under shim-in-binary's child-process model the stub and compiler are two processes — killing one stranded the other.)
- **Written in Promise.** A hello-world Promise binary is ~20 KB versus ~3 MB for the equivalent Go binary. The stub is tiny, dogfoods the language ("our launcher is written in Promise"), and compiles to a standalone native executable that needs no toolchain to run. It is built per target by the compiler at release time.

**Stub responsibilities (deliberately minimal):**
1. Resolve the target epoch: `PROMISE_EPOCH` → project `promise.toml` `[module].epoch` → `~/.promise/active`.
2. `exec` `~/.promise/epochs/<epoch>/bin/promise`, forwarding all args.
3. If that epoch is not installed → clear error (`run: promise update <epoch>`).

The stub knows only the *epoch-resolution contract* (the `active` file format and the `promise.toml` epoch key) — a small, stable surface, not the full install layout. Because it does so little, **newer stubs are guaranteed to support older compilers**, which is what makes the forward-only update rule (§2.4 step 4) safe.

> **Windows `exec` caveat.** Windows has no true `execve`; the stub there does `CreateProcess` + wait + propagate the child's exit code. The same-PID guarantee holds only on Unix. Documented so the PID/signal reasoning above is not assumed on Windows.

### 2.6 Updating *(naming change: `update` becomes self-update)*

- **`promise update`** *(planned rename)* — update **Promise itself**: download the newer compiler for the target epoch/channel and run its `install` (which forward-updates the stub and stages blobs). Today this capability lives partly in `promise sync`.
- **Dependency updates move to the package-manager namespace.** `promise update` *currently* updates `[require]` entries in `promise.toml` ([main.go:7446](../compiler/cmd/promise/main.go#L7446)); that behavior moves under a package-manager verb (e.g. `promise pkg update`) so the bare `update` can mean "update the toolchain." This is a CLI rename to schedule alongside the stub work.

Re-running install with a newer binary replaces the installation in place and forward-updates the stub.

---

## 3. Release Artifacts *(thin/full + prebuilt blobs)*

Each release publishes to **GitHub Releases** at `github.com/promise-language/promise`, tagged `epoch-YYYY.N`. See [release-automation.md](release-automation.md) for the full pipeline.

| Asset | Platform / role |
|-------|-----------------|
| `promise-linux-amd64` / `promise-linux-amd64-full` | Linux x86_64 (musl static). thin + full. |
| `promise-darwin-arm64` / `…-full` | macOS Apple Silicon. thin + full. |
| `promise-darwin-amd64` / `…-full` | macOS Intel. thin + full. |
| `promise-windows-amd64.exe` / `…-full.exe` | Windows x86_64. thin + full. *(planned)* |
| `blobs/<hash>` | Prebuilt dependency blobs (host LLVM, wasm runner, CRTs, sysroots), addressed by hash. Fetched on demand by thin binaries. |
| `SHA256SUMS` | Checksums for the top-level binary artifacts. Verified by the install scripts. |

The install script downloads only the top-level binary (and `SHA256SUMS`); everything else is fetched-by-hash at runtime against the embedded manifest.

---

## 4. The dependency store *(content-addressed cache — planned)*

```
~/.promise/cache/blobs/
  sha256/
    3f9a…/opt           ← a specific LLVM opt build
    7c21…/wasmtime
    a0e4…/libLLVM.dylib
```

**Fetch flow** when the compiler needs dependency `X`:
1. Resolve `X` → `sha256` from the embedded manifest.
2. Hit `~/.promise/cache/blobs/sha256/<hash>` → use it if present.
3. Miss → download from the release's `blobs/<hash>`, **verify the hash**, store, use.

**Offline & air-gapped.** A thin binary needs the network on first use of each dependency. Provide `promise fetch` (a.k.a. *warm*) to pre-stage the host workflow's blobs while online, and a clear error when offline and uncached ("host toolchain not cached and no network; install the `-full` build or run `promise fetch` while online"). The **full** variant is the offline guarantee.

**Garbage collection.** The store grows as epochs/targets come and go; an LRU/`promise gc` keeps it bounded. Because blobs are referenced by hash from epoch manifests, GC removes only blobs no installed epoch references.

**Build-order consequence.** A thin binary embeds the *hashes* of its prebuilts, so the prebuilts must be built and hashed **before** the compiler binary is finalized. This ordering is the central constraint of the release pipeline ([release-automation.md](release-automation.md)).

---

## 5. Platform notes

### 5.1 macOS

The compiler fetches LLVM tools (`opt`, `llc`, `lld`, `libLLVM.dylib`) as content-addressed blobs (full builds pre-stage them). On extraction, Mach-O blobs are patched and re-signed: `install_name_tool -add_rpath @loader_path`, `-change` to rewrite absolute Homebrew paths to `@rpath/<name>`, `-id @rpath/<name>`, then `codesign --force --sign -` (ad-hoc). `DYLD_LIBRARY_PATH` points the tools at the extracted dylibs.

**SDK requirement → target: zero-dep.** macOS still needs the macOS SDK sysroot for `-lSystem` (today via Xcode Command Line Tools, `xcode-select --install`). The zero-dependency goal is to bundle the minimal SDK surface (`libSystem.tbd` stubs + essential headers) as a fetched blob — like Go shipping its own linker — so a fresh macOS machine needs no prerequisites.

### 5.2 Windows

Native MSVC ABI (`x86_64-pc-windows-msvc`); `opt` → `llc` → `lld-link` against the Windows SDK + UCRT. See [windows-support.md](windows-support.md) for compiler internals.

**Zero local dependencies is required** (your install must not depend on a separately-installed, correctly-configured Visual Studio Build Tools of the right version). The Windows artifact embeds — or fetches as content-addressed blobs — the link surface it needs: UCRT/MSVC `.lib` stubs and the Windows SDK import libraries. This is the Windows analogue of the macOS SDK-stub bundling above and is a prerequisite for advertising the §2.2 one-liners.

### 5.3 Linux

Fully static via musl. The musl CRT objects are a fetched blob (full builds pre-stage them). No system dependencies beyond the kernel.

---

## 6. CI / building & publishing releases

The pull-request CI matrix (build + test per platform) and the tag-triggered release pipeline are described in **[release-automation.md](release-automation.md)**. That doc covers the new-model specifics the original §7 did not: building the prebuilt dependency **blobs**, hashing them, embedding the manifest (the build-order constraint), producing **thin + full** variants, building the **Promise stub** per target, and publishing everything to a GitHub Release on an `epoch-*` tag.

CI usage stays a one-liner:

```yaml
- name: Install Promise
  run: |
    curl -sSf https://promise-lang.org/install.sh | sh -s -- --epoch 2026.0
    echo "$HOME/.promise/bin" >> $GITHUB_PATH
```

For Docker, prefer the **full** variant so the image needs no network at build time:

```dockerfile
FROM ubuntu:24.04
COPY promise-linux-amd64-full /usr/local/bin/promise
RUN promise install
```

`promise doctor` (T0174) verifies the environment (toolchain blobs present, SDK reachable, PATH set) and exits non-zero on a missing component — useful as a CI preflight.
