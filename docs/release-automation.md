# Release Automation

> How Promise releases are built and published on GitHub. This is the pipeline behind the artifacts in [distribution.md](distribution.md) §3. It covers the new-model specifics the original distribution §7 did not: building the prebuilt dependency **blobs**, hashing them, embedding the manifest under a strict **build order**, producing **thin + full** binary variants, building the **Promise stub** per target, and publishing on an `epoch-*` tag.
>
> **Status.** The repository lives on GitHub at [`github.com/promise-language/promise`](https://github.com/promise-language/promise) (currently **private**, default branch `main`). The CI and release workflows are **committed** at [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) and [`.github/workflows/release.yml`](../.github/workflows/release.yml) (T0774) and wrap the `bin/release` driver (§7, T0773). The pipeline is now **exercised end-to-end**: CI is green on all three platforms (`linux-amd64`, `windows-amd64`, `darwin-arm64`), the `epoch-next` pre-release was cut, and the first stable release **`epoch-2026.0`** is published. CI and the release runners fetch dependency blobs **only from the `deps-<dep>-<version>` GitHub release** (`gh release download`) — the `prebuilts.promise-lang.org` R2 mirror is an end-user-install backstop, never a build source. The committed workflow files are the **source of truth**; the YAML excerpts below are the design rationale, kept in sync with them. The release *procedure* (which gates must pass before a tag) is now **enforced by `bin/release cut next` / `cut stable`** (T0943, §6): the gates run automatically and the tag/push happens only when every gate is green. The hand-run `git tag` fallback is documented in §6.2.

---

## 1. Repository & tags

[`github.com/promise-language/promise`](https://github.com/promise-language/promise) — private, default branch `main`.

| Branch | Purpose | Status |
|--------|---------|--------|
| `main` | Main development branch. All PRs target main. | exists |
| `next` | Pre-release staging — validates the next epoch before it is cut. | optional; the `epoch-next` moving tag (§6) fills this role today |

Tags follow `epoch-YYYY.N` (e.g. `epoch-2026.0`). **A tag push is a release; nothing else triggers one.** `epoch-next` is a moving pre-release tag cut at the commit being validated (see [epoch-versioned-installs.md](epoch-versioned-installs.md) §3 for channels); a dedicated `next` branch is optional and not currently used — the moving tag fills the staging role (§6). The first stable release **`epoch-2026.0`** has been cut and published; subsequent epochs follow the §6 procedure.

> **Private repo caveat.** While the repository is private, GitHub Release assets are not publicly downloadable, so the `curl … | sh` install flow in [distribution.md](distribution.md) §2 cannot work for outside users yet. Either the repo (or a releases-only mirror) must be public before the public install story is live, or releases must be served from a separate public location. Track this as a release-readiness blocker.

---

## 2. The build-order constraint

The central constraint of the new model: a **thin** binary embeds a *manifest* — per dependency, the content identity (`name`, `sha256`, `size`) plus a ranked list of **acquisition sources** ([distribution.md](distribution.md) §4.1) — and the `sha256` is the trust anchor used to verify the blob when it is acquired at runtime. The `sha256` identifies *content*, not a URL, so packaging (one-file-per-hash vs archives) is free to vary. The blobs must still exist and be hashed **before** the compiler binary that references them is finalized:

```
1. Build prebuilt blobs         (host LLVM tools, wasm runner, CRTs, target sysroots)
       ↓
2. Hash + size each blob        (sha256 of the extracted content)
       ↓
3. Package + generate manifest  (choose acquisition: direct blobs and/or archives;
                                 record name → sha256 → size → ranked sources)
       ↓
4. Build the compiler           (manifest embedded)  → thin variant
       ↓
5. Build the Promise stub        (compile with the just-built compiler, per target)
       ↓
6. Assemble the full variant     (thin binary + blobs pre-staged/embedded)
       ↓
7. Publish                       (binaries + SHA256SUMS → epoch GitHub Release;
                                  blobs live in deps-<dep>-<version>, T0797)
```

Steps 1–2 are the expensive, cacheable part (LLVM builds rarely change between epochs); per T0797 they are now done **locally on a dep version bump** (`bin/release publish-blobs`) into a dedicated `deps-<dep>-<version>` release — not on every epoch CI run. Step 3 reduces to a fast **projection** of the committed `tools/build/blobs.json` catalog (`bin/release manifest --from-catalog`); steps 4–6 are cheap and rerun every epoch.

---

## 3. Prebuilt blobs (step 1–2)

Each blob is a dependency artifact identified by the `sha256` of its extracted content, so it is immutable and shareable across epochs. **How it is published is an acquisition choice recorded in the manifest's `sources`** ([distribution.md](distribution.md) §4.1), not a fixed convention: a blob may be uploaded as its own release asset, or several blobs may ride in one compressed archive that the resolver downloads once and extracts by inner path, or a blob may be sourced from an upstream vendor archive without re-hosting. The pipeline picks the packaging; the compiler only ever asserts on the content `sha256`.

| Blob | Per | Notes |
|------|-----|-------|
| `opt`, `llc`, `lld` (+ `libLLVM`) | host platform | LLVM 22+ tools. The bulk of the size. |
| `wasmtime` / Node wasm harness | host platform | For `wasm32-wasi` / `wasm32-web` targets. |
| musl CRT objects | linux target | Static-link CRT. |
| macOS SDK stubs (`libSystem.tbd` + headers) | macOS target | Zero-dep goal ([distribution.md](distribution.md) §5.1). |
| Windows SDK / UCRT `.lib` import stubs | windows target | **Done** (T0772): self-generated license-clean import libs embedded in the compiler (~21 KiB), not a fetched blob. Zero-dep goal ([distribution.md](distribution.md) §5.2). |

**Cross-target blobs are fetched on demand by default** — a **full** host binary pre-stages only the host workflow, so targeting another platform fetches its blobs on first use. This is a packaging default, not a limit: the **all** variant ([distribution.md](distribution.md) §1.2) pre-stages every supported target's blobs as well, for fully offline cross-compilation. The runtime is identical either way — a missing blob is fetched the same way regardless of variant. *(the **all** variant is planned for later: cross-compilation is not working yet, so there are no cross-target blobs to bundle today. First releases ship thin + full only.)*

Blobs are produced **locally on a maintainer machine** (T0797), not on a per-epoch CI run. A single macOS maintainer can produce all platforms' blobs in one sitting — extracting upstream LLVM tarballs is platform-agnostic (`tar -xf`), so the linux + windows blobs come straight from the upstream tarball; only the `darwin-arm64` patch+sign step needs macOS, and the maintainer has it. The full local workflow:

```sh
bin/release publish-blobs --dependency llvm --host linux-amd64
bin/release publish-blobs --dependency llvm --host darwin-arm64
bin/release publish-blobs --dependency llvm --host windows-amd64
```

Each invocation downloads the upstream archive (already verified against `prebuilts.toml`'s pinned sha256), brotli-11-compresses each file under its content-addressed `<sha>.br` name, records the entry in `tools/build/blobs.json`, uploads the blob to a dedicated `deps-<dep>-<version>` GitHub release (created on demand), and **mirrors it to Cloudflare R2** (`--r2-bucket`, default `prebuilts`; `""` disables) as a flat `<sha>.br` object via `npx wrangler`. The catalog is the dedup oracle: a second `publish-blobs` invocation with the same dep version sees the existing catalog entry + existing release asset and skips both compress and upload. After the first run, `tools/build/blobs.json` is committed; the per-epoch pipeline reads it via `bin/release manifest --from-catalog` to project the per-epoch runtime manifest.

The committed catalog `tools/build/blobs.json` keys each hosted blob on `(dependency, version, target, name)` (e.g. `llvm / 23.1.0 / linux-amd64 / opt`) and records `sha256` (uncompressed CAS), `size`, `compression`, `compressed_size`, and optional `compressed_sha256`. It is **multi-version** (LLVM 22 and 23 coexist while dev environments migrate) and the source of truth for hosted blobs; the per-epoch runtime manifest is a **projection** of the catalog for the epoch's pinned versions. Hosting location is **derived, not stored**: `https://…/releases/download/deps-<dep>-<version>/<sha><suffix>`.

Once the repo is public (T0786), the producer can move to a `workflow_dispatch`-only CI job for auditability — still dep-bump-triggered, never per-epoch. The catalog file format is unchanged either way.

**Two hosts, two layouts.** A blob lives in **two** places. On GitHub it is a release asset at `…/releases/download/deps-<dep>-<version>/<sha>.br` — the deep path is forced by GitHub's release-asset URLs, not by us. On Cloudflare R2 it is a **flat `<sha>.br` object at the bucket root** — CAS needs no path, the sha *is* the identity. The runtime resolver fetches the GitHub primary by default; setting `PROMISE_BLOB_MIRROR=https://<host>` redirects **blob** fetches to the flat mirror `https://<host>/<sha>.br` (`rewriteBlobSource` in `blobstore/resolve.go`), while **archive** fallbacks stay path-preserving (they are upstream vendor files, not sha-named). Cloudflare is the **public backstop** that makes the whole fetch path testable while the GitHub repo is still private (T0786): `https://prebuilts.promise-lang.org` serves the `prebuilts` bucket public-read.

**macOS blobs are the raw upstream bytes** (T0769 §5.1). The hosted `darwin-arm64` blob is the *unmodified* upstream Mach-O, so its `sha256` is deterministic and verifiable **without** running `install_name_tool`/`codesign`. The patch + ad-hoc re-sign (`@loader_path` rpath; rewrite any Homebrew dylib refs to `@rpath`) is applied **only to the local loadable copy** the resolver materializes into its view dir *after* download + verify — never to the content that is hashed, so there is no patch-output determinism dependency. (The official LLVM darwin tools are statically linked against `/usr/lib` only, so in practice the patch is a near no-op; the re-sign just keeps the ad-hoc signature valid.)

**Compression — per-blob brotli (quality 11)** (T0795). Each blob is published *independently* brotli-compressed (pure-Go `github.com/andybalholm/brotli`, compiled into the compiler so it can always decode its own dependencies). The runtime resolver brotli-decompresses fetched bytes, then verifies the **uncompressed** content `sha256` (still the cache key and the file that runs) — so the content-addressed identity is unchanged; compression is purely a transport layer. The manifest source carries a `compression: "brotli"` field alongside a `compressed_size` (the over-the-wire byte count of the `.br` asset, projected from the catalog) so the runtime can report the **download** size — not the unpacked size — in its first-run fetch prompt. `compressed_size` is blob-only (rejected on `archive` sources, like `compression`); when absent the runtime falls back to the uncompressed `size`.

**`compression` → asset-suffix map.** A `blob` source's `compression` field selects the published asset's name. The stem is always the *uncompressed* content `<sha256>` (the cache key + what the runtime verifies after decompressing); the suffix encodes the transport codec. The resolver keys off the `compression` **field**, not the suffix; the suffix is for non-resolver consumers (`verify-manifest`, mirrors, humans). The local CAS file is always the bare, decompressed `<sha256>` regardless of codec.

| `compression` | published asset | codec |
|---------------|-----------------|-------|
| omitted / `"none"` | `<sha256>` | none (raw bytes) |
| `"brotli"` | `<sha256>.br` | brotli (RFC 7932) |

Only `none` and `brotli` are emitted today (brotli-11 for every fetched dependency blob). A future codec must extend this map with its conventional suffix — `"gzip"`→`.gz`, `"zstd"`→`.zst`, `"xz"`→`.xz` — kept in sync between the producer (`bin/release manifest`) and the resolver/`verify-manifest`.

Why brotli-11 and why per-blob:
- **brotli-11** won a Go-library benchmark on the LLVM tool blobs (3 darwin-arm64 blobs, 377 MB → **77 MB, 4.87×**; decompress ~1.2 s). It beats LZMA/xz (92 MB, 6.4 s decompress), bzip2, and gzip on *both* ratio and decompress, and beats `zstd --best` (96 MB) on ratio. Its only cost is slow compression (~10 min single-threaded for the LLVM set), which is acceptable because **compression is rare** (only on a new dependency version) **and cacheable** (unchanged content-hash → reuse the compressed asset, never recompress), while **download size recurs for every user/install** and is the metric to minimize.
- **Per-blob, not one combined archive:** bumping a single dependency (e.g. musl CRT) must not invalidate or force recompression of the others (e.g. all of LLVM). Each blob compresses/uploads/caches on its own content hash.

This applies to everything Promise *fetches as a dependency*. It does **not** apply to the initial `promise` binary install download, which cannot use brotli (no decompressor exists on a fresh target system before the binary is installed) — see [distribution.md](distribution.md) §2.3.

---

## 4. Build + test CI (manual dispatch)

CI is **manual `workflow_dispatch` only** — it does *not* run on push/PR. At the repo's commit volume on a private repo where Actions minutes are metered and macOS bills 10×, per-commit CI is not feasible; per-commit correctness comes from the mandatory local `bin/verify` gate, and this workflow validates the build+test toolchain on GitHub runners (which the release pipeline depends on). It builds and tests per platform — picking a single platform or `all` to control cost — and does **not** produce release artifacts (that is `release.yml`, §5). Trigger it with **`bin/release ci`** (below) or the Actions UI.

Committed at [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) (the authoritative copy). Essentials:

```yaml
name: CI
on:
  workflow_dispatch:                 # manual only — no push/PR trigger
    inputs:
      platform:  { type: choice, default: all, options: [all, linux-amd64, darwin-arm64, windows-amd64] }
      run_tests: { type: boolean, default: true }   # false = build only (cheap toolchain check)
jobs:
  test:
    strategy:
      fail-fast: false
      matrix:
        include:
          - { name: linux-amd64,   runner: ubuntu-24.04 }
          - { name: darwin-arm64,  runner: macos-latest }
          - { name: windows-amd64, runner: windows-latest }
          # - { name: darwin-amd64, runner: macos-13 }   # deferred — Intel macOS unverifiable (no working Xcode CLT); see §7
    runs-on: ${{ matrix.runner }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version-file: compiler/go.mod, cache: true, cache-dependency-path: compiler/go.sum }
      # No Java/ANTLR step: the generated parser (compiler/internal/parser/*.go) is committed.
      - name: Install LLVM + musl (Linux)
        if: runner.os == 'Linux'
        run: |
          wget -qO- https://apt.llvm.org/llvm.sh | sudo bash -s -- 22
          sudo apt-get install -y musl-dev
      - name: Install LLVM (Windows)
        if: runner.os == 'Windows'
        run: choco install llvm -y   # 22+; windows-latest already ships VS Build Tools (MSVC + Windows SDK)
      - name: Install wasmtime (runtime for the wasm32-wasi tests)
        uses: bytecodealliance/actions/wasmtime/setup@v1   # cross-platform; puts wasmtime on PATH
      # bin/ is gitignored — bootstrap the forge dev tools (bin/build, bin/test, …) before using them.
      - name: Bootstrap dev tools (Unix)
        if: runner.os != 'Windows'
        run: ./make
      - name: Bootstrap dev tools (Windows)
        if: runner.os == 'Windows'
        run: .\make.cmd
      - name: Build compiler
        run: bin/build
        env: { PROMISE_USE_CLANG: "${{ runner.os == 'macOS' && '1' || '' }}" }
      - name: Test — Go + Promise + WASM
        # Always bin/test [go|promise|all] — never `go test ./...` directly: bin/test
        # builds and embeds (stdlib, catalog) first, which raw `go test` skips.
        run: bin/test --wasm all
        env: { PROMISE_USE_CLANG: "${{ runner.os == 'macOS' && '1' || '' }}" }
```

**Triggering manually (`bin/release ci`).** The convenience trigger for the dispatch above (`tools/build/common/release_ci.go`). With no platform it dispatches **`linux-amd64` only** (the cheap default); `bin/release ci all` runs the whole matrix in one run; `bin/release ci darwin` — or `linux`/`windows`, or a canonical `<os>-<arch>` — names individual targets, and multiple names fan out to one run each. `--no-tests` sets `run_tests=false` (build-only toolchain check); `--watch` polls the dispatched run(s) to completion and **exits non-zero if CI is red** (usable as a script gate — it snapshots the latest run ID first, so it follows the run *this* dispatch creates, not a stale green one already at the same commit). Because `workflow_dispatch` can only target a branch/tag ref — never an arbitrary commit — and `actions/checkout` runs at that ref's **remote tip**, `ci` dispatches on the current branch and verifies your local HEAD **is** that pushed tip, so CI tests the commit you are on; it errors if you have not pushed (`--force` dispatches on the remote tip anyway, `--ref <branch>` picks another branch). `--commit-hash <sha>` pins CI to an exact commit (must be HEAD or an ancestor of the branch), useful when you want CI to validate a specific historical commit rather than the current HEAD. `git`/`gh` sit behind the same interface seams as `cut`, so the tests are hermetic. (`cut` dispatches the same workflow as a release-gate side effect — §6.3; `bin/release ci` is the standalone "run CI on my commit" path.)

**Platform notes:** **Bootstrap first.** `bin/` is gitignored — the dev tools (`bin/build`, `bin/gate`, `bin/release`) are [forge](https://github.com/promise-language/forge) tools compiled by `./make` (`.\make.cmd` on Windows), which also bakes the repo root into each binary and refuses to run a stale/un-bootstrapped tool. So every job runs `./make` before invoking any `bin/*` tool (see [build-tools.md](build-tools.md)). **No Java/ANTLR step** — the generated parser (`compiler/internal/parser/*.go`) is committed, so neither CI nor a release regenerates it. Whoever edits the grammar (`grammar/*.g4`) regenerates and commits the generated source in the same change (rare now). Linux installs LLVM 22 from `apt.llvm.org` and `musl-dev`. macOS uses `PROMISE_USE_CLANG=1` (Xcode clang as driver) to skip a ~5 min `brew install llvm` — same frontend/codegen, only the backend driver differs. **wasmtime** is installed on every runner (via the bytecode-alliance setup action) to run the `wasm32-wasi` tests. **Windows is a full matrix member** (no longer gated on validation): it builds with the native MSVC toolchain (`opt` → `llc` → `lld-link`, no clang), LLVM 22 via `choco`, with VS Build Tools (MSVC + Windows SDK) preinstalled on `windows-latest`, and passes the full suite (`bin/test --wasm all`) — see [windows-support.md](windows-support.md).

---

## 5. Release workflow (tag-triggered)

Triggered by an `epoch-*` tag. Implements the build order of §2 minus blob production: the per-epoch `blobs` job is gone (T0797), so `release.yml` now projects the manifest from the committed `blobs.json` catalog, pulls pre-hosted blobs on demand, and otherwise runs the same thin→stub→full→verify→publish chain.

Committed at [`.github/workflows/release.yml`](../.github/workflows/release.yml) (the authoritative copy). The as-built workflow differs from the shape below in a few mechanical ways the sketch glosses over — they are intentional: the `compiler` job installs the host LLVM toolchain (phase B's stub `-release` compile needs a backend); artifacts land under `dist/bin/` + `dist/manifest-<host>.json`; and Windows binary names carry `.exe` supplied by the workflow (not by `bin/release`). The `manifest --from-catalog` step deliberately omits `--tag` — the deps release tag is catalog-derived (`deps-<dep>-<version>`); a workflow override would point the manifest's blob URLs at a release that does not host the blobs. Shape:

```yaml
name: Release
on:
  push: { tags: ['epoch-*'] }
permissions: { contents: write }

jobs:
  # 1–6. Per-platform compiler (thin) + stub + full, with the manifest embedded.
  # NO per-epoch `blobs` job — blobs are produced locally and uploaded to
  # `deps-<dep>-<version>` (T0797). The compiler job projects the manifest from
  # the committed catalog and pulls pre-hosted blobs on demand.
  compiler:
    strategy:
      matrix: { include: [ {host: linux-amd64,   runner: ubuntu-24.04},
                           {host: darwin-arm64,  runner: macos-latest},
                           {host: windows-amd64, runner: windows-latest} ] }
                           # darwin-amd64 (macos-13) deferred — see §7
    runs-on: ${{ matrix.runner }}
    steps:
      - uses: actions/checkout@v4
      # The as-built job also installs the host LLVM toolchain here (apt.llvm.org /
      # choco / PROMISE_USE_CLANG on macOS) — phase B compiles the stub via
      # `promise build -release`, which needs a backend; the forge prebuilts cache
      # is a DIFFERENT cache and does not satisfy it. ${{ matrix.ext }} is '' / '.exe'.
      - name: Project per-epoch manifest from blobs catalog
        # No blobs need to be staged here. The runtime manifest is a projection
        # of `tools/build/blobs.json` for the epoch's pinned dep version. NO --tag
        # — the deps-<dep>-<version> tag is catalog-derived; any override would
        # point the embedded URLs at an epoch release that does not host blobs.
        run: |
          bin/release manifest --from-catalog \
            --host ${{ matrix.host }} \
            --out dist/manifest-${{ matrix.host }}.json
      - name: Build thin compiler (embed manifest + stub)
        # `bin/release build` is itself the build-order: it builds a bootstrap
        # compiler, compiles tools/stub/main.pr WITH that compiler, then rebuilds
        # with the stub embedded back in (3 internal phases). The published thin
        # binary therefore already carries the stub for install-time extraction.
        run: bin/release build --variant thin --manifest dist/manifest-${{ matrix.host }}.json --out dist/bin/promise-${{ matrix.host }}${{ matrix.ext }}
      - name: Fetch dependency blobs for full variant
        # Pull each manifest entry's blob source from the pre-staged
        # deps-<dep>-<version> release and brotli-decompress to dist/blobs,
        # keeping the compressed <sha>.br alongside (--keep-compressed) so the
        # full build embeds the brotli blob directly, no gzip recompress (T0807).
        env: { GH_TOKEN: "${{ secrets.GITHUB_TOKEN }}" }
        run: bin/release fetch-blobs --manifest dist/manifest-${{ matrix.host }}.json --out dist/blobs --keep-compressed
      - name: Assemble full variant (pre-stage host blobs)
        run: bin/release build --variant full --manifest dist/manifest-${{ matrix.host }}.json --blobs dist/blobs --out dist/bin/promise-${{ matrix.host }}-full${{ matrix.ext }}
      - uses: actions/upload-artifact@v4
        with:
          name: release-bin-${{ matrix.host }}
          path: |
            dist/bin
            dist/manifest-${{ matrix.host }}.json

  # 7. Collect everything, checksum, publish.
  publish:
    needs: [compiler]
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4   # needed to attach the committed scripts/install.*
      # ... + setup-go + ./make bootstrap (bin/release is a forge tool) ...
      - uses: actions/download-artifact@v4
        with: { pattern: release-bin-*, merge-multiple: true, path: dist/ }
      - name: Fetch dependency blobs (for integrity gate)
        # Re-pull the blobs into dist/deps with the compressed <sha>.br retained;
        # verify-manifest hashes the decompressed bytes against the manifest's
        # CAS hash. --keep-compressed is the integrity gate's contract.
        env: { GH_TOKEN: "${{ secrets.GITHUB_TOKEN }}" }
        run: |
          mkdir -p dist/deps
          for m in dist/manifest-*.json; do
            bin/release fetch-blobs --manifest "$m" --out dist/deps --keep-compressed
          done
      - name: Verify manifest resolves (fail the release on any mismatch)
        # For every manifest entry, confirm a packaged source yields bytes whose
        # sha256 matches. Catches bogus entries here so users never
        # download-and-discard at runtime (distribution.md §4.3).
        run: bin/release verify-manifest dist/manifest-*.json --against dist/deps
      - name: Compress binaries with gzip (T0796 — published assets are .gz only)
        # `-9` for max ratio; `-n` strips the embedded mtime so re-runs of the same
        # commit produce byte-identical artifacts. Drop the .sha256 sidecars first so
        # the glob only catches binaries.
        run: cd dist/bin && rm -f ./*.sha256 && for f in promise-*; do gzip -9 -n "$f"; done
      - name: Generate SHA256SUMS (over the .gz assets — what users download)
        run: cd dist/bin && sha256sum promise-*.gz > ../SHA256SUMS
      - name: Create GitHub Release
        env: { GH_TOKEN: "${{ secrets.GITHUB_TOKEN }}" }
        # The install scripts are committed (scripts/install.*, present via the checkout
        # prelude) and attached verbatim — nothing generates them. Users fetch them at
        # releases/latest/download/install.sh (distribution.md §2.1). epoch-next cuts a
        # GitHub pre-release (the `next` channel). Only .gz assets + SHA256SUMS +
        # installers are published — dependency blobs live in deps-<dep>-<version>.
        run: |
          if [ "$GITHUB_REF_NAME" = "epoch-next" ]; then
            PRERELEASE="--prerelease"; TITLE="Promise epoch-next (pre-release)"
          else
            PRERELEASE=""; TITLE="Promise epoch ${GITHUB_REF_NAME#epoch-}"
          fi
          gh release create "$GITHUB_REF_NAME" $PRERELEASE \
            --title "$TITLE" \
            --notes-from-tag \
            dist/bin/promise-*.gz dist/SHA256SUMS \
            scripts/install.sh scripts/install.ps1 scripts/install.cmd   # the installers themselves
```

Notes:
- **Release notes are `--notes-from-tag`.** `bin/release cut` writes the release notes into the annotated tag body — a mechanical, bulleted list of non-merge commit subjects in `epoch-<last>..<sha>` (newest first), generated from `git log` with no manual step. `release.yml` publishes that tag body verbatim as the GitHub release notes alongside the artifacts. There is no committed changelog file to keep in sync and no editorial gate to satisfy before a cut; a future "smart" step can enrich the notes (issue-aware titles, gate/health status) without changing how a release is cut.
- `bin/release` (T0773) is the release driver implementing the build-order. Subcommands: `blobs --host <t> --out <dir>` (collect host dependency blobs), `manifest <blobsdir> --host <t> --pack <dir> --out <m> [--tag <tag>]` (hash+size, pack hash-named upload artifacts, emit the ranked-sources manifest), `build --variant {thin|full} --manifest <m> --out <bin> [--blobs <dir>]` (the 3-phase compiler+stub build), and `verify-manifest <m>... --against <dir>` (the integrity gate). `bin/build --release` remains a shortcut that produces an embed-everything (full-equivalent) binary without the stub.
- The **stub** is compiled *by the just-built compiler* inside `bin/release build` (an internal phase), then embedded back into the compiler so `promise install` can extract it ([distribution.md](distribution.md) §2.5). Cross-compiling the stub per target is gated on cross-compilation (T0524); first releases build the host stub only.
- **Hosting:** each manifest entry's primary `source` is a **GitHub release asset** on `github.com/promise-language/promise`, named by the blob's content `sha256` (content-addressed → an unchanged dependency reuses the same asset across releases, no re-upload). The pinned upstream vendor archive (e.g. the LLVM tarball) is a ranked fallback source. A CDN/R2 mirror ([T0523](#)) is a deferred, optional future source — ranked sources + `PROMISE_BLOB_MIRROR` make adding it non-breaking (no content hashes change).
- **Install binaries are gzip-compressed only** (T0796). Each `promise-*` binary is published as `promise-*[.exe].gz` — no raw asset. Gzip is the universal floor: `gunzip` ships on every POSIX system, and Windows decompresses via `System.IO.Compression.GzipStream`. Brotli/zstd/xz can't bootstrap the *first* install (the decompressor lives inside the promise binary that doesn't exist yet); the brotli-11 path in §3 is for dependency blobs the already-installed compiler fetches at runtime — a different problem.
- **`SHA256SUMS` is computed over the `.gz` assets** — the bytes that are actually downloaded. All three consumers (`install.sh`, `install.ps1`, `promise update`/`sync`) verify the checksum before decompressing. This verifies HTTP/CDN integrity (catches a truncated/corrupted download immediately) and mirrors the normal `sha256sum *.gz` convention.
- `SHA256SUMS` covers only the top-level binaries — dependency blobs are self-verifying via their content `sha256` in the embedded manifest, regardless of how they are packaged (direct files or archives).
- `windows-amd64` is a **full matrix member** in `compiler` (CI already builds and passes the gates on it). Post-T0797 there is no per-epoch `blobs` job to be a member of — `windows-amd64` blobs are produced locally via `bin/release publish-blobs --host windows-amd64` like every other target. Its top-level artifacts carry `.exe` (`promise-windows-amd64.exe`, `…-full.exe`). The extension is supplied **by the workflow** (a `matrix.ext` field appended to `--out`), **not** by `bin/release` — the driver writes `--out` verbatim. The Windows compiler still builds via `opt` → `llc` → `lld-link` (no LTO yet — T0049).
- The **all** variant ([distribution.md](distribution.md) §1.2) is the same "assemble" step with *every* supported target's blobs in the pre-stage set instead of just the host's — no new runtime code. It is deferred until cross-compilation works (no cross-target blobs exist yet), so first releases publish thin + full only.

---

## 5a. End-to-end install gate

The end-to-end install gate (`bin/gate install --variant {thin|full} [--channel {next|stable|<epoch>}]`, [gate-system.md](gate-system.md) §Class 3) validates the **real** user install path: it fetches the published install script from a GitHub release, runs it (download → verify checksum → decompress → `promise install`), sanity-checks the install, then runs the full test suite through the freshly **installed** distribution. The repo is public, so the gate installs straight from GitHub releases — `--channel` selects which: **next** (the moving `epoch-next` pre-release, default), **stable** (the latest published epoch), or an explicit **`Y.N`** epoch. This lets the same gate validate both the pre-release before a stable cut and the stable release after it. The gate never sets `PROMISE_BASE_URL` — GitHub is its only source (T0804 done; the `prebuilts.promise-lang.org` install bucket is gone). The installer **scripts** still honor `PROMISE_BASE_URL` as a manual **testing** override (point an install at a staged mirror), but it is unset for real users and unused by the gate.

> **No longer on the install path.** The install gate now fetches from GitHub releases (above), so `bin/release publish-install` is **not** part of release validation anymore. It is retained as a manual **staging/testing** tool for an R2/dist bucket (handy for exercising the install flow against a private mirror). The former `scripts/early-access/*` installers, which served that bucket during the private-repo phase, have been removed.

`bin/release publish-install` stages an R2/dist bucket. For the host platform it:

1. projects the runtime manifest from the catalog (`manifest-<host>.json`),
2. builds the **thin** and **full** compiler variants (reusing `bin/release build`'s three-phase logic). The **full** variant embeds the already-brotli `<sha>.br` blobs fetched from the dist CAS *directly* — byte-identical to the CAS asset, no gzip recompress round trip (T0807); the runtime brotli-decompresses them into the CAS at `promise install` (`decompressEmbeddedLLVM`),
3. gzips each binary to its published asset name (`promise-<os>-<arch>[-full][.exe].gz`),
4. computes a **merge-aware** `SHA256SUMS` over the `.gz` assets, and
5. uploads the assets + `SHA256SUMS` + the `install.{sh,ps1,cmd}` scripts to `dist/` in the R2 bucket via `npx wrangler` (the same mechanism `publish-blobs` uses).

```sh
# Run once per platform. Stage all hosts into the SAME --out so SHA256SUMS
# accumulates every platform's lines (R2 has no read-modify-write, so the merge
# happens locally in --out before upload).
bin/release publish-install --host darwin-arm64  --out dist
bin/release publish-install --host linux-amd64   --out dist
bin/release publish-install --host windows-amd64 --out dist
```

Flags: `--out` (staging dir, default `<root>/dist`), `--r2-bucket` (default `prebuilts`, `""` disables upload), `--dry-run` / `--no-upload` (build + stage but skip upload). Host-only for now (cross-build gated on T0524) — the maintainer runs it on each platform, like `publish-blobs`.

**T0804 is done:** the repo is public and the gate fetches straight from GitHub releases via `--channel`. `publish-install` + the install scripts' `PROMISE_BASE_URL` override are no longer used by the gate, but are kept as a manual R2-staging/testing path (stage a bucket, then install from it with `PROMISE_BASE_URL=<mirror> install.sh`).

---

## 6. Cutting a release

A release is **a tag push** (§5): `epoch-YYYY.N` cuts a stable release, `epoch-next` cuts/refreshes the pre-release channel. The release pipeline builds and publishes but **runs no tests** — so every correctness guarantee comes from the gates *before* the tag. Those gates are now **enforced by `bin/release cut`** (§6.3, **T0943**): run `bin/release cut next` / `bin/release cut stable` and the tag/push happens only when every gate is green. §6.2 documents the equivalent hand-run `git tag` fallback the commands replace.

`epoch-next` is a **moving** tag — re-cut on every push; `release.yml` deletes + recreates its GitHub **pre-release** (the `publish` job detects `epoch-next` and passes `--prerelease`) while keeping the tag. Stable `epoch-X.Y` tags are **immutable**: `gh release create` refuses to clobber them and the workflow never deletes them, so they must never be force-moved.

### 6.1 Prerequisite — host the dependency blobs (once per dep version)

The per-epoch pipeline does not build LLVM; it pulls pre-hosted slim blobs (opt/llc/lld + the build-only llvm-dlltool) from the `deps-<dep>-<version>` GitHub release. On a dependency version bump — **or if that release is missing** (e.g. lost in a repo rebuild) — the maintainer stages the blobs locally, GitHub-only:

```sh
bin/release publish-blobs --dependency llvm --host linux-amd64   --r2-bucket ""
bin/release publish-blobs --dependency llvm --host darwin-arm64  --r2-bucket ""
bin/release publish-blobs --dependency llvm --host windows-amd64 --r2-bucket ""
```

`--r2-bucket ""` disables the Cloudflare R2 mirror: **CI and the release runners fetch blobs only from the `deps-<dep>-<version>` GitHub release.** The build-time fetcher is `gh release download` (see `release_publish_blobs.go`), never HTTP — so the GitHub release must exist and be complete, or `bin/build`/`bin/release build` fails during resource embedding (the winlink import-lib step needs `llvm-dlltool`). The R2 mirror (`prebuilts.promise-lang.org`) is an end-user-install backstop while the repo is private, not a build source. `publish-blobs` is idempotent — blobs already in `blobs.json` with a matching hash + hosted asset are skipped.

> The CI/release build steps must also set `GH_TOKEN` (job-level) so `gh release download` can authenticate on the runners — this is why the `compiler`/`test` jobs carry `env: GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}`. The read-only `GITHUB_TOKEN` suffices for downloading same-repo release assets.

### 6.2 The procedure

The steps below are **what `bin/release cut` enforces** (§6.3) — run as `bin/release cut next` then `bin/release cut stable`. The raw `git tag` snippets are the hand-run fallback the commands replace (and what a bypass with `--reason` reduces to); prefer the gated commands so no step is skipped.

1. **Stage deps blobs** (§6.1) if the `deps-<dep>-<version>` release is missing or stale. Every `blobs.json` blob for the pinned versions × all hosts must be hosted, or the build fails mid-run. (Gate 4.)
2. **Verify locally:** `bin/verify --wasm`.
3. **Catalog epoch.** Confirm `catalog.toml`'s `epoch` equals the epoch being cut (after each stable cut it is bumped to the next epoch — step 6 — so in steady state it already matches). Release notes need no manual step: `cut` generates them mechanically from the commit history in `epoch-<last>..<sha>` and embeds them in the tag (published by `release.yml` via `--notes-from-tag`). (Gate 3.)
4. **Green CI at the release head.** CI must be green on **all platforms** at the exact commit — CI is manual `workflow_dispatch` and is the *only* test coverage a release gets. `cut` checks this (gate 7) and, when no run covers the commit, offers to dispatch `ci.yml` for the missing platforms and watch them to green.
5. **Pre-release, then stable — same hash.** Cut `epoch-next` at the release commit and confirm its `release.yml` run is green (this validates the full blobs→manifest→thin→full→verify→publish chain on the exact tree), then cut stable at the *same commit* `epoch-next` validated (gate 8). Because `origin/main` keeps moving under continuous development, pin both cuts to the validated commit with `--sha <commit>` rather than relying on HEAD:

   ```sh
   bin/release cut next --sha <commit>    # gated; refreshes the moving epoch-next pre-release at <commit>
   # …confirm the epoch-next release.yml run is green…
   bin/release cut stable --sha <commit>  # gated; derives the epoch, tags <commit>, pushes, bumps catalog

   # hand-run fallback (what the commands do):
   #   git tag -f epoch-next <commit> && git push -f origin epoch-next   # moving tag; force expected
   #   git tag epoch-2026.1 <commit>  && git push origin epoch-2026.1     # immutable — never force-move a stable tag
   ```
6. **Bump for ongoing development.** `cut stable` finishes by advancing `catalog.toml`'s `epoch` to the next epoch (same-year increment) on `main` and pushing, so dev builds and the next `epoch-next` embed the upcoming epoch — not the shipped one (the side-by-side install layout dispatches on the embedded epoch).

> **Bootstrap exception — `epoch-2026.0`.** The first stable release predates the gates and could not satisfy "promote the exact hash `epoch-next` validated" (nothing preceded it). It was cut by hand after all-platform CI went green and the pipeline was proven via `epoch-next`; it ships slightly stale `os`/`http` module descriptions (corrected on `main` for `2026.1`). From `2026.1` onward every cut goes through §6.3's gates.

### 6.3 `bin/release cut` with enforced gates (T0943) — implemented

§6.2 is encoded as two gated orchestrator subcommands so neither a maintainer nor an agent can skip a step (`tools/build/common/release_cut.go`):

```sh
bin/release cut next       # refresh the epoch-next pre-release
bin/release cut stable     # derive epoch, gate, tag, push, bump catalog — all automatic
```

Flags (both channels): `--sha <commit/ref>` (pin the cut to this commit instead of HEAD — `origin/main` keeps moving under continuous development, so a cut names the commit it validated; the ref is peeled to its commit and must still be reachable from `origin/main`, gate 2), `--dry-run` (run all gates, print the checklist, change nothing), `--reason "<text>"` (override the failed *overridable* gate(s); recorded into the tag/commit message so any bypass is auditable), `--run-ci` (non-interactively dispatch `ci.yml` for platforms with no run at the release SHA), `--no-ci-wait` (with `--run-ci`: dispatch then stop, re-run `cut` once green). `cut stable` additionally takes `--confirm-year` (non-interactively confirm a year-rollover epoch). There is **no `--epoch` flag** — `cut stable` owns the number (see derivation above).

`--notes-file <path>` (or `-` to read from stdin) and `--notes "<text>"` override the mechanical git-log bullets with a hand-authored or AI-generated body. The flags are mutually exclusive. When either is given, the install header (`**Install:** …`) is always auto-prepended; the custom body follows. Nothing is committed — notes live only in the annotated tag object and are published by `release.yml` via `--notes-from-tag`. Workflow: `bin/release changes | <AI summary> | bin/release cut stable --notes-file -`. `--dry-run` prints the exact final body (header + custom notes + any gate-override reason) before cutting. Default (no flag) keeps the mechanical auto-generated notes unchanged.

Both run a preflight checklist and only tag/push when **every** gate is green. A non-overridable gate (a tool error; an already-present immutable stable tag; the `--no-ci-wait` dispatch-and-stop) aborts regardless of `--reason`. The clock-behind epoch-derivation refusal is likewise never overridable.

**Epoch derivation — `cut stable` owns the number (no `--epoch` flag):**

```
Y    = device-clock year
last = highest released epoch-<a.b> tag
  last is none      → target = Y.0          # first release
  last.year == Y    → target = Y.(b+1)      # same-year increment, no skipping N
  last.year == Y-1  → target = Y.0          # year rollover (requires confirmation)
  last.year <  Y-1  → refuse unless --reason # multi-year gap
  last.year >  Y    → refuse                # clock behind last release; never backward
```

Invariants: no N skipping within a year; the year is always the device-clock year (no jumping to a chosen future year); a year change requires explicit confirmation; `catalog.toml`'s epoch must equal `target` or the cut refuses ("catalog epoch is X, expected target") — except a confirmed year-rollover (or multi-year-gap under `--reason`), where the catalog legitimately still holds the **prior year's** dev epoch (no dev bump ever crosses the year boundary); the catalog gate accepts it, and the post-cut bump advances catalog to `Y.1` for ongoing development.

**Gate catalog:**

| # | Gate | `cut next` | `cut stable` |
|---|------|:--:|:--:|
| 1 | Clean working tree | ✓ | ✓ |
| 2 | Tag commit reachable from `origin/main` | ✓ | ✓ |
| 3 | Catalog epoch == target (refuse if off) | sanity | ✓ |
| 4 | All deps blobs hosted on `deps-<dep>-<version>` | ✓ | ✓ |
| 5 | Epoch monotonic + rule-valid (no skip/jump) | – | ✓ |
| 6 | Stable tag/release not already present | n/a (force-move) | ✓ |
| 7 | Green CI on **all platforms** at this SHA (see below: *absent* vs *failed*) | ✓ | ✓ |
| 8 | `epoch-next` tag == this SHA **and** its release run succeeded | – | ✓ |

After the gates pass, both channels generate the release notes from `git log epoch-<last>..<sha>` (mechanical, non-merge commit subjects) and embed them in the annotated tag, so `release.yml` can publish them with `--notes-from-tag`. There is no changelog gate: notes are a *product* of the cut, never a precondition — so a moving `origin/main` never forces a CI re-run to re-pin a changelog edit.

`cut stable` then: tag → push → commit the catalog bump (`Y.(N+1)`) on `main` → push.

Gate 7 (all-platform green CI) applies to **both** channels — a pre-release users could install must be fully built+tested first. Gate 8 enforces *validate-via-next-then-promote-the-same-hash*.

**Gate 7 — absent vs. failed CI.** The gate distinguishes a *missing* run from a *failed* one, because a missing run is a fixable gap, not a red signal:

- **No CI run covers this SHA** (for one or more required platforms) → **do not refuse outright**. On an interactive terminal, prompt: *"CI has not run at `<sha>` for `<platforms>`. Dispatch `ci.yml` now? [y/N]"*. On yes, the tool dispatches `ci.yml` for the missing platforms, watches them to completion, and re-evaluates the gate. Non-interactively, the same is opted into with `--run-ci` (and `--no-ci-wait` to dispatch-and-exit, instructing the maintainer to re-run `cut` once green); without either, it aborts with a message naming the missing platforms rather than a bare refusal.
- **CI ran and any required platform failed** → **refuse** (real signal). This is *not* auto-re-run; it is overridable only with `--reason "<text>"`, recorded into the tag/commit message.

This keeps the happy path (CI already green) instant, turns the common "forgot to run CI" case into a one-keystroke dispatch, and still hard-stops on an actual CI failure.

**Inputs.** Every gate input is reachable from `bin/release`: `git` (tree, reachability, tags, `rev-parse` for `--sha`, `log` for the release notes), `gh` (CI run + per-job conclusions by `headSha`; `deps-*` release assets via the existing `releaseUploader`; the `epoch-next` release run), and a `catalog.toml` parse. The implementation puts `git`/`gh` behind the `cutGit` / `cutGH` interfaces (the same stub pattern as `releaseUploader` / `blobFetcher`), so `release_cut_test.go` is fully hermetic — no `git`/`gh` process is spawned and the CI watch loop's sleep is stubbed.

### 6.4 `bin/release changes` — release preview (T1141)

A read-only convenience command for previewing what would go into the next stable release's notes before cutting:

```sh
bin/release changes                    # subjects from last stable epoch tag to HEAD
bin/release changes --commit-hash <sha>  # same range but upper bound pinned to <sha>
```

**Behavior.** Resolves the last stable `epoch-*` tag (same logic as `cut stable`: `highestReleasedEpoch` excludes `epoch-next` and any non-numeric tags). Prints a one-line header then one commit subject per line (non-merge commits only, newest first — same `git log --no-merges --pretty=format:%s` primitive `cut` uses for release notes).

```
42 commits since epoch-2026.1
T1141: bin/release changes subcommand
T1139: Move-param enum-ctor temp claim over-claims intermediate borrow temps
…
```

**`--commit-hash`**: pins the upper bound to an exact SHA; validated as HEAD-or-ancestor of HEAD (mirrors `ci --commit-hash`). Use this to preview the precise range for a SHA you intend to pass to `cut --sha <sha>`.

**No prior epoch**: when no stable `epoch-*` tag exists, the range covers the entire history from the upper bound, and the header reads `N commits (no prior stable epoch)`.

**AI-authored release notes workflow** (pairs with the companion `cut --notes-file` task): pipe `bin/release changes` output into an AI summarizer, then feed the summary into `bin/release cut stable --notes-file <file>` to override the mechanical bullet list with a human-readable description.

This command is **read-only**: no gates, no tagging, no side effects beyond `git fetch --tags` to ensure local epoch tags are current. Implemented in `tools/build/common/release_changes.go`.

---

## 7. Open items

| Item | Notes |
|------|-------|
| ~~Gated release orchestrator~~ (done, **T0943**) | The §6.2 procedure is encoded as `bin/release cut next` / `cut stable` (`tools/build/common/release_cut.go`): the gates (catalog-epoch match, all-platform green CI at the head with absent-vs-failed handling, validate-via-next-then-promote-same-hash, deps blobs hosted, rule-valid auto-derived epoch + post-cut catalog bump) are enforced by the tool, not by discipline, and release notes are generated mechanically from the commit history into the tag (`--notes-from-tag`). `--dry-run` reports without changing anything; `--reason` audits any override. Design + shipped flags: §6.3. `epoch-2026.0` is grandfathered (predates the gates); the rules apply from `2026.1` onward. |
| ~~`bin/release` driver~~ (done, T0773) | The blob/hash/manifest/thin/full/stub steps + `verify-manifest` gate are implemented as a Go build tool alongside `bin/build` (`tools/build/cmd/release`, `tools/build/common/release*.go`). |
| Blob hosting & packaging | **Decided (T0773):** primary `source` is a one-file-per-hash **GitHub release asset** (named by content `sha256`); the upstream vendor archive is a ranked fallback. A dedicated CDN/bucket ([T0523](#)) is a deferred optional source. `PROMISE_BLOB_MIRROR` base-URL override ([epoch-versioned-installs.md](epoch-versioned-installs.md) §3) and the *ranked* source list let the private→public transition add/promote a public source without changing content hashes (§1 private-repo caveat). |
| Private→public release access (**T0786**) | While the repo is private, **nothing in the install path is anonymously fetchable**: the install scripts are themselves release assets (`releases/latest/download/install.sh`), and the binaries + dependency blobs they pull are too — all need auth or a public mirror. Resolve before advertising the public install (§1, [distribution.md](distribution.md) §2.1). Tracked as the standalone release-readiness blocker **T0786** (`needs-attention`); ties to T0523's public-origin requirement. |
| ~~Manifest integrity gate~~ (done, T0773) | `bin/release verify-manifest <m>... --against <dir>` resolves every entry against the packaged artifacts (hashing a blob asset, or extracting `archive_path` from an archive) and **fails the release** on any `sha256` mismatch or missing artifact, so a bogus entry never reaches users ([distribution.md](distribution.md) §4.3). |
| Mismatch telemetry (opt-in) | Decide whether to ship the opt-in integrity-mismatch signal ([distribution.md](distribution.md) §4.4): what it sends (dependency, source, expected/actual hash, epoch, platform), the disclosure/opt-in UX, and where it reports. Integrity-only, never general usage. |
| ~~Blob caching across releases~~ (done, T0773) | Content-addressed packaging: `bin/release manifest` names each upload artifact by its `sha256`, so an unchanged dependency version (hence unchanged hash) is left untouched in the pack dir — no rebuild/re-upload. (Steps 1–2 also reuse the prebuilts cache's unchanged-hash skip.) |
| ~~Per-epoch blob production~~ (done, T0797) | Blobs are produced **locally** by a maintainer (`bin/release publish-blobs`) on a dependency version bump, recorded in the committed `tools/build/blobs.json` catalog, and hosted in a dedicated `deps-<dep>-<version>` release. The per-epoch pipeline projects the catalog (`bin/release manifest --from-catalog`) and pulls pre-hosted blobs (`bin/release fetch-blobs`) — no 700 MB upstream LLVM download, no 10-min brotli-11 on every release. |
| Windows release artifact | CI is done — `windows-amd64` is a full matrix member passing the gates. LLVM embed (T0056) and the self-generated SDK / UCRT import stubs for the zero-dep "no VS Build Tools required" goal (T0772, [windows-support.md](windows-support.md)) are both **done**. The only remaining release-side gap is LTO (T0049, deferred). |
| `darwin-amd64` (Intel) — **deferred** | Dropped from the CI/release matrices: the maintainer can't run a working Xcode CLT on available Intel hardware, so the target can't be verified. The build code exists; revisit if a verifiable Intel runner/host is available (GitHub's `macos-13` Intel runner could validate it in CI even without local hardware — reconsider before deletion). |
| `linux-arm64` | Cross-compile + arm64 runner; arm64 LLVM blobs + musl CRT. |
| Stub cross-build | Confirm the host compiler can emit the stub for every shipped target. |
| `all` variant | Add once cross-compilation works: extend the "assemble" step to pre-stage every supported target's blobs ([distribution.md](distribution.md) §1.2). Release-packaging only — no runtime change. Gated on cross-compilation landing. |
