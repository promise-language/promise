# Release Automation

> How Promise releases are built and published on GitHub. This is the pipeline behind the artifacts in [distribution.md](distribution.md) §3. It covers the new-model specifics the original distribution §7 did not: building the prebuilt dependency **blobs**, hashing them, embedding the manifest under a strict **build order**, producing **thin + full** binary variants, building the **Promise stub** per target, and publishing on an `epoch-*` tag.
>
> **Status (2026-05-30).** The PR CI matrix and a baseline single-artifact release workflow are designed (and partly in use). The thin/full + blob + stub pipeline below is the **target** and is not yet wired up. The repository is on a local git server today; these GitHub Actions workflows are written to drop in when it moves to `github.com/promise-language/promise`.

---

## 1. Repository & tags

```
github.com/promise-language/promise
```

| Branch | Purpose |
|--------|---------|
| `main` | Main development branch. All PRs target main. |
| `next` | Pre-release staging — validates the next epoch before it is cut. |

Tags follow `epoch-YYYY.N` (e.g. `epoch-2026.0`). **A tag on `main` is a release; nothing else triggers one.** A pre-release tagged `epoch-next` tracks the `next` branch (see [epoch-versioned-installs.md](epoch-versioned-installs.md) §3 for channels).

---

## 2. The build-order constraint

The central constraint of the new model: a **thin** binary embeds a *manifest of `(name, sha256, size, fetch-coords)`* for every heavy dependency, and the `sha256` is the trust anchor used to verify the blob when it is fetched at runtime ([distribution.md](distribution.md) §1.1, §4). Therefore the blobs must exist and be hashed **before** the compiler binary that references them is finalized:

```
1. Build prebuilt blobs         (host LLVM tools, wasm runner, CRTs, target sysroots)
       ↓
2. Hash + size each blob        (sha256)  → these are the content addresses
       ↓
3. Generate the manifest        (name → sha256 → size → blobs/<hash> URL)
       ↓
4. Build the compiler           (manifest embedded)  → thin variant
       ↓
5. Build the Promise stub        (compile with the just-built compiler, per target)
       ↓
6. Assemble the full variant     (thin binary + blobs pre-staged/embedded)
       ↓
7. Publish                       (binaries + blobs + SHA256SUMS → GitHub Release)
```

Steps 1–2 are the expensive, cacheable part (LLVM builds rarely change between epochs). Steps 3–6 are cheap and rerun every release.

---

## 3. Prebuilt blobs (step 1–2)

Each blob is a single dependency artifact, published as a release asset named by its content hash (`blobs/<sha256>`), so it is immutable and shareable across epochs.

| Blob | Per | Notes |
|------|-----|-------|
| `opt`, `llc`, `lld` (+ `libLLVM`) | host platform | LLVM 22+ tools. The bulk of the size. |
| `wasmtime` / Node wasm harness | host platform | For `wasm32-wasi` / `wasm32-web` targets. |
| musl CRT objects | linux target | Static-link CRT. |
| macOS SDK stubs (`libSystem.tbd` + headers) | macOS target | Zero-dep goal ([distribution.md](distribution.md) §5.1). |
| Windows SDK / UCRT `.lib` import stubs | windows target | Zero-dep goal ([distribution.md](distribution.md) §5.2). |

**Cross-target blobs are always fetched on demand** — they are published as blobs but never bundled into a "full" host binary, because no binary can carry every target's toolchain ([distribution.md](distribution.md) §1.2).

Blobs are produced by their own infrequently-run workflow (or a cached job), keyed so that an unchanged LLVM version reuses the previous hash and is not rebuilt or re-uploaded.

---

## 4. PR CI

Runs on every push to `main`/`next` and every PR. Builds and tests per platform — it does **not** produce release artifacts.

`.github/workflows/ci.yml` (essentials):

```yaml
name: CI
on:
  push: { branches: [main, next] }
  pull_request: { branches: [main, next] }
jobs:
  test:
    strategy:
      fail-fast: false
      matrix:
        include:
          - { name: linux-amd64,  runner: ubuntu-24.04 }
          - { name: darwin-arm64, runner: macos-latest }
          - { name: darwin-amd64, runner: macos-13 }
          # - { name: windows-amd64, runner: windows-latest }   # add after E2E validation
    runs-on: ${{ matrix.runner }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version-file: compiler/go.mod, cache: true, cache-dependency-path: compiler/go.sum }
      - uses: actions/setup-java@v4
        with: { distribution: temurin, java-version: 21 }
      - name: Cache ANTLR JAR
        uses: actions/cache@v4
        with: { path: compiler/tools/antlr-4.13.1-complete.jar, key: antlr-4.13.1 }
      - name: Install LLVM + musl (Linux)
        if: runner.os == 'Linux'
        run: |
          wget -qO- https://apt.llvm.org/llvm.sh | sudo bash -s -- 22
          sudo apt-get install -y musl-dev
      - name: Build
        run: bin/build
        env: { PROMISE_USE_CLANG: "${{ runner.os == 'macOS' && '1' || '' }}" }
      - name: Go tests
        working-directory: compiler
        run: go test ./... -count=1
      - name: Promise tests
        run: bin/test promise
        env: { PROMISE_USE_CLANG: "${{ runner.os == 'macOS' && '1' || '' }}" }
```

**Platform notes:** Linux installs LLVM 22 from `apt.llvm.org` and `musl-dev`. macOS uses `PROMISE_USE_CLANG=1` (Xcode clang as driver) to skip a ~5 min `brew install llvm` — it exercises the same frontend/codegen, only the backend driver differs. Windows is added to the matrix after end-to-end validation (see [windows-support.md](windows-support.md)).

---

## 5. Release workflow (tag-triggered)

Triggered by an `epoch-*` tag. Implements the build order of §2: resolve/build blobs → generate manifest → build thin compiler → build stub → assemble full → publish.

`.github/workflows/release.yml` (shape):

```yaml
name: Release
on:
  push: { tags: ['epoch-*'] }
permissions: { contents: write }

jobs:
  # 1–2. Prebuilt blobs (cached; rebuilt only when a dependency version changes).
  blobs:
    strategy:
      matrix: { include: [ {host: linux-amd64, runner: ubuntu-24.04},
                           {host: darwin-arm64, runner: macos-latest},
                           {host: darwin-amd64, runner: macos-13} ] }
    runs-on: ${{ matrix.runner }}
    steps:
      - uses: actions/checkout@v4
      - name: Build/collect dependency blobs (LLVM, wasm runner, CRT, sysroot stubs)
        run: bin/release blobs --host ${{ matrix.host }} --out dist/blobs
      - name: Hash blobs
        run: bin/release hash dist/blobs > dist/manifest-${{ matrix.host }}.json   # name→sha256→size→url
      - uses: actions/upload-artifact@v4
        with: { name: blobs-${{ matrix.host }}, path: dist/ }

  # 3–6. Per-platform compiler (thin) + stub + full, with the manifest embedded.
  compiler:
    needs: [blobs]
    strategy:
      matrix: { include: [ {host: linux-amd64, runner: ubuntu-24.04},
                           {host: darwin-arm64, runner: macos-latest},
                           {host: darwin-amd64, runner: macos-13} ] }
    runs-on: ${{ matrix.runner }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/download-artifact@v4
        with: { name: blobs-${{ matrix.host }}, path: dist/ }
      - name: Build thin compiler (embed manifest)
        run: bin/release build --variant thin --manifest dist/manifest-${{ matrix.host }}.json --out dist/promise-${{ matrix.host }}
      - name: Build the Promise stub for this target
        run: dist/promise-${{ matrix.host }} build --release tools/stub/main.pr -o dist/stub-${{ matrix.host }}
        # the stub is embedded back into the compiler for extraction at install time
      - name: Assemble full variant (pre-stage host blobs)
        run: bin/release build --variant full --manifest dist/manifest-${{ matrix.host }}.json --blobs dist/blobs --out dist/promise-${{ matrix.host }}-full
      - uses: actions/upload-artifact@v4
        with: { name: compiler-${{ matrix.host }}, path: dist/ }

  # 7. Collect everything, checksum, publish.
  publish:
    needs: [compiler]
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/download-artifact@v4
        with: { path: dist/ }
      - name: Generate SHA256SUMS (top-level binaries only)
        run: cd dist && sha256sum promise-* > SHA256SUMS
      - name: Create GitHub Release
        env: { GITHUB_TOKEN: "${{ secrets.GITHUB_TOKEN }}" }
        run: |
          EPOCH="${GITHUB_REF_NAME#epoch-}"
          gh release create "$GITHUB_REF_NAME" \
            --title "Promise epoch ${EPOCH}" \
            --notes "See docs/changelog.md for this epoch." \
            dist/promise-* dist/blobs/* dist/SHA256SUMS
```

Notes:
- `bin/release` is the (planned) release driver that wraps the build-order steps; today the equivalent is `bin/build --release` producing a single embed-everything binary.
- The **stub** is compiled *by the just-built compiler* (step 5), then embedded back into the compiler so `promise install` can extract it ([distribution.md](distribution.md) §2.5). Cross-compiling the stub per target is done by the host compiler.
- `SHA256SUMS` covers only the top-level binaries — blobs are self-verifying via their content-address hashes in the embedded manifest.
- `windows-amd64` is added as additional `blobs`/`compiler` matrix entries once Windows is validated.

---

## 6. Cutting a release

```sh
# 1. Verify locally (formats, vets, full test suite incl. wasm)
bin/verify --wasm

# 2. Tag and push
git tag epoch-2026.0
git push origin epoch-2026.0
```

The tag push triggers §5. No manual binary uploads, no manual checksum computation. To stage a pre-release for the `next` channel, push `epoch-next` (marked as a GitHub pre-release).

---

## 7. Open items

| Item | Notes |
|------|-------|
| `bin/release` driver | Implement the blob/hash/manifest/thin/full/stub steps as a Go build tool alongside `bin/build`. |
| Blob hosting layout | `blobs/<sha256>` as release assets vs a dedicated CDN/bucket; configurable base URL for mirrors / air-gap ([epoch-versioned-installs.md](epoch-versioned-installs.md) §3). |
| Blob caching across releases | Skip rebuild/upload when a dependency version (hence hash) is unchanged. |
| `windows-amd64` | Add to the `blobs` + `compiler` matrices after end-to-end validation ([windows-support.md](windows-support.md)). |
| `linux-arm64` | Cross-compile + arm64 runner; arm64 LLVM blobs + musl CRT. |
| Stub cross-build | Confirm the host compiler can emit the stub for every shipped target. |
