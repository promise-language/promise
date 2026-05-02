# Distribution & Installation

> The self-contained binary model is implemented for Linux (amd64). macOS is partially implemented (binary is portable but requires Xcode Command Line Tools). Windows support is planned but not yet available. Multi-epoch toolchain management (`promise sync`, shim) is described in `docs/module-system.md` Section 7.

---

## 1. Design

The Promise binary is fully self-contained. A single executable contains:
- The compiler
- The standard library (`std/*.pr`)
- The LLVM toolchain (`opt`, `llc`, `lld`, compressed via `go:embed`)
- The musl CRT (Linux static builds)

No system dependencies are required beyond the OS itself (Linux) or Xcode Command Line Tools (macOS). Users download the binary and run `promise install`, which sets everything up.

---

## 2. Installation

### 2.1 Linux & macOS — install script

```sh
curl -sSf https://promise-lang.dev/install.sh | sh
```

This downloads `scripts/install.sh` from the CDN (backed by the GitHub release for the latest stable epoch). The script:

1. Detects OS and architecture
2. Downloads the matching binary from GitHub Releases
3. Verifies the SHA256 checksum
4. Runs `./binary install` — which does the actual setup (see §2.3)
5. Prints PATH instructions

To install a specific epoch:

```sh
curl -sSf https://promise-lang.dev/install.sh | sh -s -- --epoch 2026.3
```

### 2.2 Direct download

The install script is a thin wrapper. Users can also install manually:

```sh
# Linux amd64
curl -LO https://github.com/promise-lang/promise/releases/latest/download/promise-linux-amd64
chmod +x promise-linux-amd64
./promise-linux-amd64 install
```

`promise install` handles everything from here.

### 2.3 What `promise install` does

`promise install` (`runInstall()` in `cmd/promise/main.go`) copies and extracts itself into the Promise home directory (default `~/.promise/`, overridable via `PROMISE_HOME`):

```
~/.promise/
  bin/
    promise          ← the binary (copied from the downloaded file)
    llvm/            ← LLVM tools extracted from the binary (Linux)
  lib/
    std/             ← embedded standard library source
    crt/             ← musl CRT objects (Linux)
```

It then prints the PATH export line. Users add `~/.promise/bin` to their `PATH` once; all future `promise` invocations use the installed binary.

### 2.4 Updating

Re-running `promise install` with a newer binary replaces the installation in place. For epoch upgrades, `promise sync` (described in `docs/module-system.md` §7) downloads the correct binary for a given epoch and calls the equivalent of `promise install` on it. Until multi-epoch support is implemented, re-running the install script with `--epoch` is the update mechanism.

### 2.5 Windows

Windows support is planned but not yet implemented. The model is identical — a self-contained `promise.exe` embedding the compiler, stdlib, and LLVM tools; `promise install` sets up `%USERPROFILE%\.promise\`. Until then, Windows users can use WSL2 with the Linux binary.

---

## 3. Release Artifacts

Each release publishes to **GitHub Releases** at `github.com/promise-lang/promise`, tagged `epoch-YYYY.N`.

| Binary | Platform |
|--------|----------|
| `promise-linux-amd64` | Linux x86_64, fully static (musl). Implemented. |
| `promise-linux-arm64` | Linux ARM64. Planned. |
| `promise-darwin-amd64` | macOS Intel. Needs Xcode CLT. Planned. |
| `promise-darwin-arm64` | macOS Apple Silicon. Needs Xcode CLT. Planned. |
| `promise-windows-amd64.exe` | Windows x86_64. Planned. |

Each release also includes a `SHA256SUMS` file. `scripts/install.sh` verifies the checksum before running `promise install`.

---

## 4. macOS Notes

The macOS binary requires **Xcode Command Line Tools** for linking:

```sh
xcode-select --install
```

This provides `ld` (the Mach-O linker). `opt` and `llc` are embedded in the binary (same as Linux). The error message when CLT is missing is:

```
no Mach-O linker found
  install Xcode CommandLineTools: xcode-select --install
  or set PROMISE_USE_CLANG=1 to use clang
```

**Planned:** embed `opt`, `llc`, `ld64.lld`, and `libLLVM.dylib` in the macOS release binary — the same gzip + `go:embed` pattern used on Linux (`llvm_linux_amd64.go`). This requires adding `llvm_darwin_amd64.go` and `llvm_darwin_arm64.go` with matching build tags, and extending `make llvm-bundle` to run on macOS. Once done, macOS will be fully self-contained with no Xcode CLT dependency, matching the Linux experience. Tracked in `docs/stages.md` (Near-term).

---

## 5. Install Script Location

Two install scripts exist with different purposes:

| Script | Purpose |
|--------|---------|
| `scripts/install.sh` | **End-user installer.** Downloads a release binary from GitHub and runs `promise install`. Served at `promise-lang.dev/install.sh`. |
| `bin/install.sh` | **Developer installer.** Builds a release binary from source (`./build --release`) then runs `promise install`. Used when iterating on the compiler itself. |

Both scripts ultimately delegate to `promise install` for the actual filesystem setup.

---

## 6. CI/CD

The standard install script works in CI:

```yaml
# GitHub Actions example
- name: Install Promise
  run: |
    curl -sSf https://promise-lang.dev/install.sh | sh -s -- --epoch 2026.3
    echo "$HOME/.promise/bin" >> $GITHUB_PATH
```

For Docker, copy the binary into the image directly — no install script needed:

```dockerfile
FROM ubuntu:24.04
COPY promise-linux-amd64 /usr/local/bin/promise
RUN promise install
```

`promise doctor` (planned — see `docs/stages.md`) can be used in CI to verify the environment before running builds or tests:

```yaml
- run: promise doctor --json   # exits non-zero if any required component is missing
```

---

## 7. GitHub Infrastructure

This section describes the GitHub repository setup, CI, and release process. The project is currently on a local git server; these workflows are ready to drop in when it moves to GitHub.

### 7.1 Repository

```
github.com/promise-lang/promise
```

| Branch | Purpose |
|--------|---------|
| `main` | Main development branch. All PRs target main. |
| `next` | Pre-release staging. Used to validate the next epoch before cutting. |

Tags follow the format `epoch-YYYY.N` (e.g., `epoch-2026.3`). A tag on main is a release. Nothing else triggers a release.

### 7.2 CI Workflow

Runs on every push to `main` and every pull request. Tests on all currently supported platforms.

`.github/workflows/ci.yml`:

```yaml
name: CI

on:
  push:
    branches: [main, next]
  pull_request:
    branches: [main, next]

jobs:
  test:
    name: Test (${{ matrix.name }})
    runs-on: ${{ matrix.runner }}
    strategy:
      fail-fast: false
      matrix:
        include:
          - name: linux-amd64
            runner: ubuntu-24.04
          - name: darwin-arm64
            runner: macos-latest
          - name: darwin-amd64
            runner: macos-13

    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: compiler/go.mod
          cache: true
          cache-dependency-path: compiler/go.sum

      - uses: actions/setup-java@v4
        with:
          distribution: temurin
          java-version: 21

      - name: Cache ANTLR JAR
        uses: actions/cache@v4
        with:
          path: compiler/tools/antlr-4.13.1-complete.jar
          key: antlr-4.13.1

      # LLVM 22+ is required. ubuntu-24.04 ships LLVM 18; install 22 from apt.llvm.org.
      - name: Install LLVM + musl (Linux)
        if: runner.os == 'Linux'
        run: |
          wget -qO- https://apt.llvm.org/llvm.sh | sudo bash -s -- 22
          sudo apt-get install -y musl-dev

      # macOS runners have Xcode CLT pre-installed. PROMISE_USE_CLANG=1 uses
      # clang as the driver, avoiding a ~5min `brew install llvm` in CI.
      # Release builds use the full LLVM pipeline (see release workflow).
      - name: Build
        run: ./build
        env:
          PROMISE_USE_CLANG: ${{ runner.os == 'macOS' && '1' || '' }}

      - name: Go tests
        working-directory: compiler
        run: go test ./... -count=1

      - name: Promise tests
        run: bin/test.sh promise
        env:
          PROMISE_USE_CLANG: ${{ runner.os == 'macOS' && '1' || '' }}
```

**Platform notes:**
- **Linux**: LLVM 22 installed from `apt.llvm.org`. `musl-dev` provides the musl CRT objects that get embedded in the binary by `make musl-crt`.
- **macOS**: `PROMISE_USE_CLANG=1` uses Xcode's bundled clang as the compilation driver, avoiding the `brew install llvm` overhead. This tests the same compiler frontend and codegen — only the backend driver differs.
- **Windows**: Not yet. Will be added to the matrix once `promise install` supports Windows.

### 7.3 Release Workflow

Triggered by a tag push matching `epoch-*`. Builds one binary per platform, collects them in a final job, generates `SHA256SUMS`, and creates the GitHub Release.

`.github/workflows/release.yml`:

```yaml
name: Release

on:
  push:
    tags:
      - 'epoch-*'

permissions:
  contents: write   # needed for gh release create

jobs:
  # ── per-platform builds ──────────────────────────────────────────────────

  build-linux-amd64:
    name: Build linux-amd64
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: compiler/go.mod
          cache: true
          cache-dependency-path: compiler/go.sum
      - uses: actions/setup-java@v4
        with:
          distribution: temurin
          java-version: 21
      - name: Cache ANTLR JAR
        uses: actions/cache@v4
        with:
          path: compiler/tools/antlr-4.13.1-complete.jar
          key: antlr-4.13.1
      - name: Install LLVM + musl
        run: |
          wget -qO- https://apt.llvm.org/llvm.sh | sudo bash -s -- 22
          sudo apt-get install -y musl-dev
      # Release build: embeds LLVM tools + musl CRT → fully self-contained ~61MB binary.
      - name: Build (release)
        run: ./build --release
      - uses: actions/upload-artifact@v4
        with:
          name: promise-linux-amd64
          path: bin/promise

  build-darwin-arm64:
    name: Build darwin-arm64
    runs-on: macos-latest    # Apple Silicon
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: compiler/go.mod
          cache: true
          cache-dependency-path: compiler/go.sum
      - uses: actions/setup-java@v4
        with:
          distribution: temurin
          java-version: 21
      - name: Cache ANTLR JAR
        uses: actions/cache@v4
        with:
          path: compiler/tools/antlr-4.13.1-complete.jar
          key: antlr-4.13.1
      # macOS release binary: compiler + stdlib embedded. LLVM tools NOT embedded yet
      # (embed_llvm is Linux-only for now). Binary requires Xcode CLT at runtime.
      - name: Build
        run: ./build
      - uses: actions/upload-artifact@v4
        with:
          name: promise-darwin-arm64
          path: bin/promise

  build-darwin-amd64:
    name: Build darwin-amd64
    runs-on: macos-13         # Intel
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: compiler/go.mod
          cache: true
          cache-dependency-path: compiler/go.sum
      - uses: actions/setup-java@v4
        with:
          distribution: temurin
          java-version: 21
      - name: Cache ANTLR JAR
        uses: actions/cache@v4
        with:
          path: compiler/tools/antlr-4.13.1-complete.jar
          key: antlr-4.13.1
      - name: Build
        run: ./build
      - uses: actions/upload-artifact@v4
        with:
          name: promise-darwin-amd64
          path: bin/promise

  # ── collect + publish ────────────────────────────────────────────────────

  release:
    name: Publish release
    needs: [build-linux-amd64, build-darwin-arm64, build-darwin-amd64]
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4

      - name: Download all binaries
        uses: actions/download-artifact@v4
        with:
          path: dist/

      # Artifacts are unpacked into dist/<artifact-name>/<filename>.
      # Rename each to its final release name.
      - name: Rename binaries
        run: |
          mv dist/promise-linux-amd64/promise  dist/promise-linux-amd64
          mv dist/promise-darwin-arm64/promise dist/promise-darwin-arm64
          mv dist/promise-darwin-amd64/promise dist/promise-darwin-amd64
          chmod +x dist/promise-linux-amd64 dist/promise-darwin-arm64 dist/promise-darwin-amd64

      - name: Generate SHA256SUMS
        working-directory: dist/
        run: sha256sum promise-linux-amd64 promise-darwin-arm64 promise-darwin-amd64 > SHA256SUMS

      - name: Create GitHub Release
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          EPOCH="${GITHUB_REF_NAME#epoch-}"
          gh release create "$GITHUB_REF_NAME" \
            --title "Promise epoch ${EPOCH}" \
            --notes "See [changelog](docs/changelog.md) for what changed in this epoch." \
            dist/promise-linux-amd64 \
            dist/promise-darwin-arm64 \
            dist/promise-darwin-amd64 \
            dist/SHA256SUMS
```

### 7.4 Cutting a Release

When the codebase is ready for a new epoch:

```sh
# 1. Verify everything passes locally
bin/verify.sh

# 2. Tag the commit
git tag epoch-2026.3
git push origin epoch-2026.3
```

That's it. The tag push triggers the release workflow, which builds all platform binaries and creates the GitHub Release automatically. No manual binary uploads, no manual SHA256 computation.

### 7.5 Planned Platform Additions

| Platform | Blocker | Notes |
|----------|---------|-------|
| `linux-arm64` | Cross-compile + arm64 runner | Go cross-compiles fine. Musl CRT arm64 objects needed. `embed_llvm` needs arm64 LLVM tools bundled. |
| `windows-amd64` | `promise install` Windows support | Once implemented, add a `build-windows-amd64` job on `windows-latest`. The Go binary cross-compiles from Linux with `GOOS=windows`. |
