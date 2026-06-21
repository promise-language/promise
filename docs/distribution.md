# Distribution & Installation

> **Status.** This document describes the **target** distribution architecture. Some of it is implemented today, some is planned; each section is marked. The headline change from the original model is twofold: (1) heavy dependencies (LLVM tools, wasm runner, CRTs, target sysroots) move from *embedded-in-the-binary* to a *content-addressed cache fetched on demand*, so the compiler ships in **thin** and **full** variants that behave identically; and (2) the on-`PATH` entry point becomes a **tiny stub** (written in Promise, extracted at install) instead of a full copy of the compiler. See §1 for the model, §7 / [release-automation.md](release-automation.md) for how releases are built and published.
>
> **Implemented:** self-contained `--release` binary (embeds *everything* — LLVM, musl CRT, stdlib) on Linux (amd64) and macOS arm64 (Intel/amd64 builds exist but are **deferred** — unverifiable without working Xcode CLT); `promise install` with the epoch layout; epoch dispatch via *shim-in-binary*; `promise update`/`promise use` to fetch and activate epochs. **Planned:** thin/full split + content-addressed dependency store (§1, §4); the embedded Promise stub replacing shim-in-binary (§2.5); `promise update` self-update rename (§2.6); `promise install <epoch>` fetch-without-activate + corrected missing-epoch recovery hint (§2.5 step 4, §2.6 — T0977); Windows end-user install flow (§5.2). Windows compiler support is complete, including the zero-dependency link surface — no VS Build Tools / Windows SDK required (see [windows-support.md](windows-support.md)).

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

The binary embeds a **manifest** — one entry per heavy dependency, separating **content identity** (`logical name`, `sha256`, `size`) from **acquisition** (a ranked list of sources describing *how* to obtain it). The `sha256` identifies the blob's *content* — it is the cache key and the integrity check — **not** a download URL. At the moment a dependency is required, the compiler looks it up in the shared content-addressed cache (`~/.promise/cache/blobs/sha256/<hash>`); if present it is used, if absent it is acquired by trying the manifest's sources in order, **verified against the embedded `sha256`**, cached, and used. That hash is the **trust anchor** — an acquired blob needs no separate signature, only a content match. The acquisition layer is deliberately flexible (a source may be a direct download *or* a path inside a compressed archive); see §4 for the entry shape and why this matters for the private→public transition.

### 1.2 Variants (thin / full / all)

A variant differs only in **which blobs ship pre-staged into the cache**. The embedded compiler and the runtime fetch mechanism (§4) are *identical* across all variants — any blob not already in the cache is acquired the same way. The variant just changes how often that happens. So this is a packaging knob, a point on a spectrum, not three different programs:

| Variant | Pre-staged blobs | Size (approx.) | Network |
|---------|------------------|---------------|---------|
| **thin** | none (manifest only) | ~20 MB | fetches every blob on first use |
| **full** | host default workflow (host LLVM + host CRT) | ~150 MB | none for normal host use; cross-target fetched on demand |
| **all** *(planned, later)* | every supported target's blobs too | largest (hundreds of MB) | none, ever — offline for all targets including cross-compilation |

(The **all** variant is informally "super".) `promise build hello.pr` on a thin binary fetches host LLVM the first time and is identical thereafter; the full and all builds already have it.

**"Full" is host-workflow by default because bundling every target is large — not because of any mechanism limit.** Targeting `wasm32-wasi` or `linux-arm64` from a macOS host fetches those target blobs on a thin or full binary, and finds them already pre-staged on an **all** binary. Behavior is identical either way; only the disk-vs-network tradeoff differs. The **all** build is the choice for fully air-gapped cross-compilation and long-term archival ("must still work in 5 years, for every target, with no network").

> **The `all` variant is designed now, shipped later.** Cross-compilation is not working yet, so cross-target blobs — and therefore the **all** variant — have nothing to pre-stage today. The point of describing it here is that the *mechanism requires no change*: an **all** build is just a full build with the cross-target blobs added to the pre-stage set; any blob that is missing is still fetched identically (§4). First releases ship **thin** and **full** only; the **all** variant is added once cross-compilation lands (it's a release-packaging step, not new runtime code).

### 1.3 Why content-addressing (not per-epoch copies)

A content-addressed store **deduplicates across epochs and targets**. Two installed epochs built on the same LLVM 22 share **one** cached copy keyed by hash, instead of each epoch carrying its own `bin/llvm/`. Epoch directories shrink to *references* (which blob hashes they need); `promise remove <epoch>` becomes "drop this epoch's references, then GC blobs referenced by *no* installed epoch." That "no installed epoch" qualifier is load-bearing: because blobs are shared, GC must be rooted at the union of **all** installed epochs' manifests so removing one never deletes a blob another still uses — see §4's GC mechanism. This supersedes the per-epoch `bin/llvm/` layout in [epoch-versioned-installs.md](epoch-versioned-installs.md).

---

## 2. Installation

### 2.1 Linux & macOS — install script *(implemented; download semantics unchanged)*

```sh
curl -sSfL https://github.com/promise-language/promise/releases/latest/download/install.sh | sh
```

**Where the script comes from.** The install scripts are **published as GitHub release assets** by the release pipeline ([release-automation.md](release-automation.md) §5) — they are the committed `scripts/install.*` files, attached to every release (nothing *generates* them). `releases/latest/download/install.sh` is GitHub's stable redirect to the newest release's copy; a `promise-lang.org/install.sh` vanity redirect can front it later. Anonymous fetch requires the repo to be public — the same gate as the binaries (§1).

The script ([scripts/install.sh](../scripts/install.sh)) detects OS/arch, downloads the matching binary (**thin** by default) from the same release, verifies its SHA256 against `SHA256SUMS`, and runs `./binary install` (§2.4). It supports `--epoch` today; the `--full` / `--all` variant flags (which select the `-full` / `-all` asset suffixes, §2.3) are planned:

```sh
BASE=https://github.com/promise-language/promise/releases/latest/download
curl -sSfL $BASE/install.sh | sh -s -- --epoch 2026.0
curl -sSfL $BASE/install.sh | sh -s -- --full      # planned (selects the -full asset)
```

**`PROMISE_BASE_URL` override (testing only).** When the env var `PROMISE_BASE_URL` is
set, both `install.sh` and `install.ps1` **skip** GitHub release resolution and download
the assets (`<asset>.gz`, `SHA256SUMS`) directly from that base URL. This is a manual
**testing** hook for installing from a staged mirror — e.g. an R2/dist bucket produced by
`bin/release publish-install` ([release-automation.md](release-automation.md) §5a):

```sh
PROMISE_BASE_URL=https://<your-mirror>/dist sh install.sh --full
```

`--epoch` is ignored under the override (the mirror is unversioned). It is unset for real
users — GitHub releases are the only install source. `install.cmd` does **not** honor it
(it always fetches `install.ps1` from GitHub).

### 2.2 Windows — install script *(scripts committed; anonymous fetch gated on private→public)*

> The committed [`scripts/install.ps1`](../scripts/install.ps1) (real implementation) and [`scripts/install.cmd`](../scripts/install.cmd) (thin shim) are attached to every release by the pipeline ([release-automation.md](release-automation.md) §5). As with §2.1, the `irm … | iex` one-liner only works for outside users once the repo (or a releases-only mirror) is public — the scripts and the binaries they fetch are release assets that currently require auth.

Windows needs **two one-liners**, one per shell, because the PowerShell idiom does not work in `cmd.exe` (`irm`/`iex` are PowerShell cmdlets — pasting them into `cmd.exe` yields `'irm' is not recognized`). This mirrors the Claude CLI installer:

```powershell
# PowerShell
irm https://github.com/promise-language/promise/releases/latest/download/install.ps1 | iex
```

```bat
:: cmd.exe (curl.exe ships with Windows 10 1803+)
curl -fsSL https://github.com/promise-language/promise/releases/latest/download/install.cmd -o install.cmd && install.cmd && del install.cmd
```

(Same hosting as §2.1 — `install.ps1` / `install.cmd` are committed in `scripts/` and attached to each release; a `promise-lang.org` vanity redirect can front them later.)

`install.ps1` is the real implementation (platform/arch detection, download, checksum, `promise install`, **User `PATH` via `[Environment]::SetEnvironmentVariable(..., 'User')`**). `install.cmd` is a **thin shim** that re-invokes PowerShell (`powershell -ExecutionPolicy Bypass -Command "irm … | iex"`) so there is a single real implementation. Direct download (§2.3) is the no-script fallback for locked-down environments.

**Zero local dependencies is the bar — met on Windows (T0772).** The Windows
compiler embeds its own self-generated link surface (own import libs from
license-clean `.def` symbol lists + codegen-emitted crt0/TLS/`__chkstk`), so
installing on a fresh Windows machine "just works" with no "install Visual Studio
Build Tools ≥ version X first" and no Microsoft `.lib` redistribution. macOS will
embed its SDK stubs the analogous way (§5). See §5.2.

### 2.3 Direct download *(implemented)*

Published assets are **gzip-compressed** (T0796) — the `.gz` suffix follows the variant and any `.exe`. The decompressed binary name is the same `promise-<os>-<arch>[-<variant>][.exe]` shape (the **bare** name is the **thin** variant; the variant suffix goes *after* the target triple and before any `.exe`):

| Variant | Linux asset / runtime name | Windows asset / runtime name |
|---------|----------------------------|------------------------------|
| thin (default) | `promise-linux-amd64.gz` / `promise-linux-amd64` | `promise-windows-amd64.exe.gz` / `promise-windows-amd64.exe` |
| full | `promise-linux-amd64-full.gz` / `promise-linux-amd64-full` | `promise-windows-amd64-full.exe.gz` / `promise-windows-amd64-full.exe` |
| all *(planned, §1.2)* | `promise-linux-amd64-all.gz` | `promise-windows-amd64-all.exe.gz` |

`SHA256SUMS` is computed over the **`.gz` asset** — the bytes that are downloaded — and the install scripts / `promise update` verify the checksum before decompressing.

Gzip is the universal compressor: `gunzip` ships on every POSIX system, and Windows decompresses via the built-in `System.IO.Compression.GzipStream`. Brotli/zstd/xz would chicken-and-egg the bootstrap (the decompressor lives *inside* the promise binary, which is what we're trying to install). The brotli-11 compression in §4 / [release-automation.md](release-automation.md) §3 is a different path — for dependency blobs the *already-installed* promise binary fetches at runtime.

```sh
# thin (default — smallest; fetches the host toolchain on first use)
curl -LO https://github.com/promise-language/promise/releases/latest/download/promise-linux-amd64.gz
gunzip promise-linux-amd64.gz
chmod +x promise-linux-amd64
./promise-linux-amd64 install

# full (offline for the host workflow) — note the -full suffix AFTER the target
curl -LO https://github.com/promise-language/promise/releases/latest/download/promise-linux-amd64-full.gz
gunzip promise-linux-amd64-full.gz
chmod +x promise-linux-amd64-full
./promise-linux-amd64-full install
```

`promise install` handles everything from here, regardless of variant.

### 2.4 What `promise install` does *(install flow updated for the stub + blob model)*

`promise install` (`runInstall()` in [cmd/promise/main.go](../compiler/cmd/promise/main.go)) installs into the Promise home directory (default `~/.promise/`, overridable via `PROMISE_HOME`):

1. **Determine the embedded epoch** from the embedded catalog.
2. **Install the compiler** → `~/.promise/epochs/<epoch>/bin/promise` (move/copy self).
3. **Stage dependencies**: a *full* binary unpacks its bundled blobs into the content-addressed cache (`~/.promise/cache/blobs/sha256/<hash>`); a *thin* binary records the manifest so they are fetched on first use.
4. **Extract the stub — forward-only** → `~/.promise/bin/promise`. The stub is replaced **only if the embedded stub's version is newer** than the installed one — read from the `~/.promise/bin/.promise-stub-version` sidecar (a file read, *not* by executing the stub, which would trampoline; §2.5), and rewritten atomically alongside the stub. Stubs are never downgraded.
5. **Write `~/.promise/active`** with the current epoch.
6. **Set `PATH`**: print the export line (Unix) or set the User `PATH` env var (Windows).

Resulting layout:

```
~/.promise/
  bin/
    promise               ← the tiny stub (Promise-compiled, forward-updated)
    .promise-stub-version ← installed stub version (read by install — never executes the stub; §2.5)
  active                  ← "2026.0"
  epochs/
    2026.0/bin/promise   ← the real compiler for this epoch
  cache/
    blobs/sha256/<hash>  ← content-addressed dependency store (shared)
    build/               ← compile cache
```

### 2.5 The stub (launcher) *(implemented — replaced shim-in-binary, T0770)*

The on-`PATH` `~/.promise/bin/promise` is the **stub**: the thing the user runs, which does *not* compile anything itself — it locates the correct epoch's real compiler and hands off to it. (Synonyms seen elsewhere: *shim*, *launcher*, *trampoline*; they all mean this one object.)

The stub is a tiny Promise-written binary ([tools/stub/main.pr](../tools/stub/main.pr)), built per target by the just-built compiler at release time (T0773). The previous *shim-in-binary* design — a full compiler copy that detected its stub role via a `.promise.shim` marker and re-exec'd the epoch binary — has been removed (`shimDispatch`, the `PROMISE_NO_SHIM` recursion guard, and the marker are all gone). The difference is not only disk and cold-start — it is a real simplification and reliability win:

> **Dev builds.** The per-target stub is a release artifact, so `bin/build` dev binaries have no embedded stub; `promise install` then places the compiler itself at the launcher path (it simply runs as the compiler — there is no shim to mediate). Multi-epoch dispatch only matters for released installs.

- **The compiler stopped being a trampoline.** `shimDispatch()`, the `PROMISE_NO_SHIM` recursion guard, the `.promise.shim` marker, and every "am I a stub or the compiler?" branch are gone. **You get what you run**: invoking the big binary directly always runs the compiler, full stop — no surprise hand-off. If the directly-invoked compiler's epoch differs from the project's pinned epoch, that is a **warning** ([epoch_warn.go](../compiler/cmd/promise/epoch_warn.go)), not a silent trampoline to something unexpected (suppress with `PROMISE_NO_EPOCH_WARN`).
- **The stub `exec`-replaces itself** with the real compiler (Unix `execve`; Windows uses the closest available, see caveat). Same process, same PID. This eliminates a whole class of bugs: record-the-PID / run / kill-the-PID no longer leaves the real compiler alive as an orphan, and signals reach the compiler directly. (Under shim-in-binary's child-process model the stub and compiler are two processes — killing one stranded the other.)
- **Written in Promise.** A hello-world Promise binary is ~20 KB versus ~3 MB for the equivalent Go binary. The stub is tiny, dogfoods the language ("our launcher is written in Promise"), and compiles to a standalone native executable that needs no toolchain to run. It is built per target by the compiler at release time.

**Stub responsibilities (deliberately minimal):**
1. **Check its reserved env vars first.** If `PROMISE_STUB_VERSION` is set, the stub prints its own version and exits; `PROMISE_STUB_INFO` additionally prints the resolved epoch and the target binary it *would* exec. These are checked *before* epoch resolution. Using an **env var, not a flag**, keeps the stub a pure pass-through that **never parses args** — it forwards every argument untouched — and matches how it reads `PROMISE_EPOCH`. So `promise --version` still trampolines to the active compiler, as users expect; you opt into stub introspection explicitly via the env var.
2. Resolve the target epoch: `PROMISE_EPOCH` → project `promise.toml` `[module].epoch` → `~/.promise/active`.
3. `exec` `~/.promise/epochs/<epoch>/bin/promise`, forwarding all args.
4. If that epoch is not installed → clear error directing the user to an **explicit** recovery: `run: promise install <epoch>` (stage it without changing the active epoch — §2.6) **or** edit `[module].epoch` in `promise.toml` to an already-installed epoch. The stub **never** auto-downloads the pinned epoch: `promise.toml` is attacker-controllable, so silently fetching+installing+executing the pinned toolchain on `promise build` would let a crafted repo run a known-vulnerable epoch. Auto-install on build is off by default, permanently — recovery is always an explicit user step. *(planned: T0977 — the stub today still prints the stale `promise update <epoch>` hint, which fails since `update` is forward-only; T0977 corrects the text and adds `install <epoch>`.)*

The stub knows only the *epoch-resolution contract* (the `active` file format and the `promise.toml` epoch key) plus its own `PROMISE_STUB_*` env vars — a small, stable surface, not the full install layout. Because it does so little, **newer stubs are guaranteed to support older compilers**, which is what makes the forward-only update rule (§2.4 step 4) safe.

**Stub versioning & the forward-only rule.** §2.4 step 4 replaces the stub only when the installer's embedded stub is *newer* than the installed one — so the installer must discover the installed stub's version. It **cannot** rely on running the stub for this: the stub forwards args, so `~/.promise/bin/promise --version` trampolines to the active *compiler* and reports the compiler's version; and `PROMISE_STUB_VERSION` only works if the *installed* stub already understands it — an older stub that predates the var would just trampoline. Therefore:
- **The install decision reads a sidecar file, never executes the stub.** Whenever the installer writes the stub it atomically writes its version to `~/.promise/bin/.promise-stub-version` next to it; §2.4 step 4 compares its embedded version against that file with a plain read. This is robust against an installed stub that is older, broken, or missing — and it is the only way to honor "never downgrade", since you cannot compare against a version you cannot read.
- **`PROMISE_STUB_VERSION=1 promise` is for humans and `promise doctor`** — a way to ask the *running* stub directly — *not* the mechanism the installer relies on.

> **Windows `exec` caveat.** Windows has no true `execve`; the stub there does `CreateProcess` + wait + propagate the child's exit code. The same-PID guarantee holds only on Unix. Documented so the PID/signal reasoning above is not assumed on Windows.

### 2.6 Updating *(implemented: `update` is self-update, T0770; channel model, T0825)*

The update channel (which release stream `update` follows) is **orthogonal** to the
active epoch (which compiler runs builds). The channel is persisted in
`~/.promise/channel` (default `stable`); the active epoch in `~/.promise/active` (T0825).

- **`promise update`** — update **Promise itself**: follow the persisted channel
  (`stable` → latest tagged `epoch-*`; `next` → the rolling `epoch-next` pre-release),
  download the channel's latest compiler, run its `install` (forward-updates the stub,
  stages blobs), and **auto-activate** the freshly installed epoch.
- **`promise update check [--json]`** — report whether an update is available without
  mutating anything. Stable staleness compares epoch tags numerically; the rolling
  `next` channel compares the platform asset's sha256 (recorded at install as
  `epochs/<epoch>/build-id`) against the remote `SHA256SUMS`.
- **`promise update channel [stable|next]`** — print the channel, or set it and
  immediately follow it.
- **`promise install <epoch>`** — *(planned: T0977)* make a specific/historical epoch
  **present on disk without activating it**: download + stage `epochs/<epoch>/`, leaving
  `~/.promise/active` untouched. This is the install-vs-activate split (rustup's `toolchain
  install` vs `default`): because epoch resolution prefers a project's `promise.toml` pin
  over the active epoch (§2.5 step 2), a pinned project only needs its epoch **present** —
  it never needs to be active. This is the recovery the missing-epoch error points at
  (§2.5 step 4), and it avoids the global side effects of activation (a later `promise init`
  scaffolding the now-active epoch; other *unpinned* projects silently switching toolchains).
  **`promise install` with no argument is unchanged — it is the bootstrap installer (§2.4),
  a different operation on the critical install path.**
- **`promise use <epoch>`** — activate a specific/historical epoch (= `install <epoch>`
  **+ set active**), **downloading it on demand** if not installed. The shared
  download+verify+install machinery lives in
  [update.go](../compiler/cmd/promise/update.go) (`downloadAndInstall`), reused by `update`,
  `use`, and `install <epoch>`. (`sync` is deleted — no separate side-by-side install command.)
- **Dependency updates moved to the package-manager namespace.** The old `promise update` behavior — updating `[require]` git-dependency pins in `promise.toml` — is now `promise package update [url]`, so the bare `update` means "update the toolchain." The broader package fetch/resolve/lock surface is tracked under T0175.

Re-running install with a newer binary replaces the installation in place and forward-updates the stub.

---

## 3. Release Artifacts *(thin/full + prebuilt blobs)*

Each release publishes to **GitHub Releases** at `github.com/promise-language/promise`, tagged `epoch-YYYY.N`. See [release-automation.md](release-automation.md) for the full pipeline. Asset names follow `promise-<os>-<arch>[-<variant>][.exe].gz` — bare prefix = **thin**, `-full`, `-all` (variant suffix after the target; see §2.3). Binaries are **gzip-compressed only** (no raw asset is published — T0796).

| Asset (bare prefix = thin / `-full` / `-all`) | Platform / role |
|-------|-----------------|
| `promise-linux-amd64.gz` / `-full.gz` / `-all.gz` | Linux x86_64 (musl static). thin + full; all *(planned)*. |
| `promise-darwin-arm64.gz` / `-full.gz` / `-all.gz` | macOS Apple Silicon. thin + full; all *(planned)*. |
| `promise-darwin-amd64.gz` | macOS Intel — *deferred* (unverifiable without working Xcode CLT; see [release-automation.md](release-automation.md) §7). |
| `promise-windows-amd64.exe.gz` / `-full.exe.gz` / `-all.exe.gz` | Windows x86_64. thin + full; all *(planned)*. |
| dependency blobs | Prebuilt dependencies (host LLVM, wasm runner, CRTs, sysroots) referenced by content `sha256` in the manifest. Acquired on demand by thin binaries. **How they're packaged is an acquisition detail** — one-file-per-hash assets, or a few compressed archives that each yield many blobs (§4). Not assumed to be one named download per hash. |
| `SHA256SUMS` | Checksums for the top-level **`.gz` assets** (what's downloaded — verified before decompressing). |

The install script downloads only the top-level binary (and `SHA256SUMS`); every dependency is acquired at runtime against the embedded manifest and verified by content `sha256` (§4).

---

## 4. The dependency store *(content-addressed cache — planned)*

```
~/.promise/cache/
  blobs/sha256/
    3f9a…/opt           ← a specific LLVM opt build (the source of truth)
    7c21…/wasmtime
    a0e4…/libLLVM.dylib
  archives/sha256/
    1b7c…                ← a cached archive (bandwidth optimization only; §4.2 Archive reuse)
```

### 4.1 Manifest entry: content identity vs acquisition

Each manifest entry separates *what the blob is* from *where/how to get it*. The `sha256` is the content address — the cache key and the integrity check — and is **decoupled from the download**. Acquisition is a **ranked list of sources**, tried in order until one yields bytes matching `sha256`. A source is either a **direct blob** or a **path inside a compressed archive**:

```jsonc
{
  "name":   "llvm-opt",            // logical name the compiler asks for
  "sha256": "3f9a…",               // content address of the EXTRACTED blob — cache key + integrity
  "size":   41234567,              // extracted (unpacked) size, bytes
  "sources": [                     // ranked; first that verifies wins
    { "blob": "https://github.com/promise-language/promise/releases/download/epoch-2026.0/3f9a….br",
      "compression": "brotli",     // transport codec of the asset
      "compressed_size": 8123456 },// over-the-wire download size — what the first-run prompt reports
                                   // primary: GitHub release asset named by content sha256
    { "archive": "https://…/LLVM-22.1.0-macOS-ARM64.tar.xz",
      "archive_path": "bin/opt",   // fallback: path to extract from *inside* the upstream archive
      "archive_sha256": "1b7c…" }  // optional: verify the archive before extracting
  ]
}
```

This is exactly the shape the producer `bin/release manifest` emits (T0773): the primary source is a **GitHub release asset** on `github.com/promise-language/promise`, named by the blob's content `sha256`; the pinned upstream vendor archive is the ranked fallback (so a not-yet-published release still resolves, and the thin compiler can bootstrap LLVM straight from upstream).

**Do not assume one `sha256` == one download named by that hash.** Several blobs may share a single archive (one LLVM tarball yields `opt`, `llc`, `lld`, `libLLVM`): the resolver downloads such an archive **once** and extracts each needed `archive_path` from it. A blob may also be sourced directly. The packaging is free to change per release without touching the compiler, because the compiler only ever asserts on the content `sha256`.

**Why the flexibility matters — especially private→public.** Multiple ranked sources let one manifest serve several acquisition paths at once: while the repo is private (§ release-automation), a source can point at the authenticated private release (or a temporary bucket) and a public mirror/CDN can be added or promoted later — same content `sha256`, new path. It also allows sourcing straight from an upstream vendor archive (e.g. an official LLVM release tarball) rather than re-hosting every tool. A runtime mirror/base override (e.g. `PROMISE_BLOB_MIRROR`) can additionally rewrite source hosts for corporate mirrors and air-gapped installs without rebuilding the binary.

### 4.2 Fetch flow

When the compiler needs dependency `X`:
1. Resolve `X` → manifest entry (`sha256` + ranked `sources`).
2. Hit `~/.promise/cache/blobs/sha256/<sha256>` → use it if present (trusted by presence for speed, not re-hashed per build; integrity is verified and repaired by `promise doctor`, see *Integrity & self-healing* below).
3. Miss → walk `sources` in order:
   - **blob**: download the bytes.
   - **archive**: download the archive (coalesced — one archive fetched once per resolution pass; optionally verified against `archive_sha256` *before* extracting), then extract `archive_path`. Whether the archive is *kept* for reuse across runs depends on `archive_sha256` — see *Archive reuse* below.
   - **verify** the resulting bytes against `sha256`. On match, go to step 4. On **mismatch**, handle it loudly (§4.3), then try the next source.
4. Store the verified bytes at `~/.promise/cache/blobs/sha256/<sha256>` and use them.

**Archive reuse.** Within one resolution pass a shared archive is fetched once and every blob that references it is extracted from that single download. *Across* runs and epochs the archive is **persistently cached only when its `archive_sha256` is given** — that asserted content address is what makes cross-run reuse safe and verifiable, so the archive is stored at `~/.promise/cache/archives/sha256/<archive_sha256>` and a later first-time extraction of another member skips the re-download. Without `archive_sha256` there is no trustworthy content key, so the archive is used within the pass and then discarded (the per-blob `sha256` still verifies every extracted blob either way). Two refinements: on first touch the resolver may extract *all* manifest blobs that reference the same archive — when the members are all wanted (e.g. the host LLVM tools) this materializes every blob and the archive needn't be kept at all; and cached archives are GC'd like blobs, **evicted first once all their referenced blobs are materialized** (at that point the archive is dead weight).

### 4.3 Content mismatch is loud, never silent

A source whose bytes don't match the expected `sha256` is a real defect — almost always a **bogus manifest entry** or a wrong/corrupted published artifact — and it has just cost the user a possibly large (≈170 MB) download for nothing. We never want that discarded silently while everyone falls through to the next source. On mismatch the resolver must:

- **Warn with full detail** — dependency name, the source URL, expected vs actual `sha256`, and the bytes wasted. Cheap defenses first: if the manifest's `size` is known, abort a download that overshoots it; verify `archive_sha256` before paying extraction cost.
- **Emit an opt-in integrity-mismatch telemetry signal** (§4.4) so a broken release is detected centrally within minutes rather than via scattered user reports — this is exactly the "downloaded, but the content isn't what I expected" case worth reporting.
- **Negative-cache the bad source for the run** so the same wrong bytes are not re-downloaded repeatedly within one build.
- If **all sources fail**, hard-error naming the dependency and that the manifest or release is likely broken (suggest `promise doctor`) — never proceed as if the dependency were absent-but-optional.

**The real fix is upstream:** the mismatch path is a safety net, not the norm. The release pipeline verifies every manifest entry against the actually-published artifacts *before* the release is cut ([release-automation.md](release-automation.md) §5), so a bogus entry never ships in the first place.

### 4.4 Telemetry (opt-in, integrity-only)

Consistent with Promise's no-hidden-effects stance, mismatch telemetry is **opt-in and disclosed**, never on by default. When enabled it sends only the **integrity-mismatch** signal — dependency name, source, expected/actual `sha256`, epoch, platform — *not* general usage or build contents, and exists solely to surface broken releases fast. Treated as a design candidate (open item), not an assumed feature.

**Offline & air-gapped.** A thin binary needs the network on first use of each dependency. Provide `promise fetch` (a.k.a. *warm*) to pre-stage the host workflow's blobs while online, and a clear error when offline and uncached ("host toolchain not cached and no network; install the `-full` build or run `promise fetch` while online"). The **full** variant is the offline guarantee.

**Garbage collection.** The store grows as epochs/targets come and go; an LRU/`promise gc` keeps it bounded. GC is **mark-and-sweep rooted at *every* installed epoch**, and getting the root set right is a **correctness requirement, not an optimization**: a blob is typically shared (two epochs on the same LLVM 22 reference the same hash), so removing one epoch must never delete a blob another still references. For GC to know the true live set it must read the referenced-blob set of *all* installed epochs:

- **Each epoch materializes its references at install.** `promise install` writes the epoch's blob/archive hash set into its own directory (e.g. `epochs/<epoch>/blobs.refs`, derived from the embedded manifest) so GC can compute roots **without executing any epoch binary** (which would be slow and fragile).
- **Live set = union of all `epochs/*/blobs.refs`.** GC sweeps only `blobs/`/`archives/` entries in *no* installed epoch's ref set. `promise remove <epoch>` deletes the epoch directory (dropping its refs) and then sweeps against the **remaining** epochs — so an epoch's *exclusive* blobs are reclaimed while *shared* blobs stay.
- **Fail safe, and locked.** GC takes the same exclusive lock as install/fetch so it cannot race a half-installed epoch whose `blobs.refs` isn't on disk yet; and if any epoch's ref set is missing or unreadable, GC **keeps** (never sweeps) rather than risk deleting a live blob — over-retention is recoverable, over-deletion wedges that epoch.

Cached archives (`cache/archives/sha256/`) are pure bandwidth optimization — never the source of truth — so GC may drop them more aggressively than blobs (the per-blob `sha256` simply re-fetches), preferring to evict an archive whose referenced blobs are all already materialized; the same all-epochs union rule decides when an archive is unreferenced.

**Integrity & self-healing — a corrupt CAS must never be unrecoverable.** For speed the cache trusts entries by *presence* — step 2 of the fetch flow does not re-hash on every hit — so on-disk corruption (bit rot, a partial write, a truncated file) would otherwise be used forever and **permanently break every build, with no way out**. Two safeguards keep that from becoming a wedged toolchain:
- **Atomic, verified writes.** The resolver downloads/extracts to a temp path, checks the `sha256`, and only then renames into `blobs/sha256/<hash>` (or `archives/sha256/<hash>`). An interrupted fetch can never leave a half-written entry that looks valid by presence.
- **`promise doctor` verifies the CAS and repairs it.** It re-hashes every entry in `blobs/` and `archives/` and compares against its content address (the directory/file name), then **removes or quarantines any entry that fails** so the next use re-fetches a clean copy. Corruption is therefore always recoverable: `promise doctor [--repair]` turns a broken cache back into a working one instead of leaving the compiler permanently wedged. This check is the integrity counterpart to GC (which only reclaims space) and to the release-time manifest gate (§4.3, which guards the *source* rather than the *cache*).

**Build-order consequence.** A thin binary embeds the *hashes* of its prebuilts, so the prebuilts must be built and hashed **before** the compiler binary is finalized. This ordering is the central constraint of the release pipeline ([release-automation.md](release-automation.md)).

---

## 5. Platform notes

### 5.1 macOS

The compiler fetches LLVM tools (`opt`, `llc`, `lld`, `libLLVM.dylib`) as content-addressed blobs (full builds pre-stage them). On extraction, Mach-O blobs are patched and re-signed: `install_name_tool -add_rpath @loader_path`, `-change` to rewrite absolute Homebrew paths to `@rpath/<name>`, `-id @rpath/<name>`, then `codesign --force --sign -` (ad-hoc). `DYLD_LIBRARY_PATH` points the tools at the extracted dylibs.

**SDK requirement → target: zero-dep.** macOS still needs the macOS SDK sysroot for `-lSystem` (today via Xcode Command Line Tools, `xcode-select --install`). The zero-dependency goal is to bundle the minimal SDK surface (`libSystem.tbd` stubs + essential headers) as a fetched blob — like Go shipping its own linker — so a fresh macOS machine needs no prerequisites.

### 5.2 Windows

Native MSVC ABI (`x86_64-pc-windows-msvc`); `opt` → `llc` → `lld-link` against a
**self-generated link surface**. See [windows-support.md](windows-support.md) §3.3
for compiler internals.

**Zero local dependencies — done (T0772).** Installing on a fresh Windows machine
with **no Visual Studio Build Tools and no Windows SDK** links runnable `.exe`s.
Rather than re-host Microsoft's `.lib` files (licensing-unclear, fetch-dependent),
Promise **generates its own link surface**: MSVC-ABI import libraries for
kernel32 / advapi32 / ws2_32 / ucrtbase, built from license-clean symbol-list
`.def` files via `llvm-dlltool` (symbol→DLL mappings are not copyrightable, so the
`.lib`s are freely re-hostable), plus a codegen-emitted crt0 (`@__promise_start`),
TLS directory (`_tls_used`), `__chkstk`, and `_fltused`. The import libs are tiny
(~21 KiB) and `go:embed`-ed into the compiler (extracted to the cache at link
time), like the embedded musl CRT objects on Linux. The audit showed ~95% of the
external surface is always-present OS DLLs + dynamically-linkable ucrtbase.dll;
only the program entry and the thread launcher needed the static MSVC CRT, and
both are self-supplied. No Microsoft toolchain file is re-hosted, repackaged, or
fetched. (x86_64 first; arm64 is a follow-up.)

### 5.3 Linux

Fully static via musl. The musl CRT objects are a fetched blob (full builds pre-stage them). No system dependencies beyond the kernel.

---

## 6. CI / building & publishing releases

The pull-request CI matrix (build + test per platform) and the tag-triggered release pipeline are described in **[release-automation.md](release-automation.md)**. That doc covers the new-model specifics the original §7 did not: building the prebuilt dependency **blobs**, hashing them, embedding the manifest (the build-order constraint), producing **thin + full** variants, building the **Promise stub** per target, and publishing everything to a GitHub Release on an `epoch-*` tag.

CI usage stays a one-liner:

```yaml
- name: Install Promise
  run: |
    curl -sSfL https://github.com/promise-language/promise/releases/latest/download/install.sh | sh -s -- --epoch 2026.0
    echo "$HOME/.promise/bin" >> $GITHUB_PATH
```

For Docker, prefer the **full** variant so the image needs no network at build time:

```dockerfile
FROM ubuntu:24.04
COPY promise-linux-amd64-full /usr/local/bin/promise
RUN promise install
```

`promise doctor` (T0174) verifies the environment (toolchain blobs present **and content-valid**, SDK reachable, PATH set), **repairs a corrupted content-addressed cache** by evicting any `blobs/`/`archives/` entry that fails its content hash (§4 *Integrity & self-healing*), and exits non-zero on a missing or unrepairable component — useful as a CI preflight.
