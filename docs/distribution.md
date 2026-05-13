# Distribution & Installation

> The self-contained binary model is implemented for Linux (amd64) and macOS (arm64 + amd64). Linux embeds LLVM tools + musl CRT for fully static binaries. macOS embeds LLVM tools (opt, llc, lld, libLLVM.dylib) but still requires Xcode Command Line Tools for the macOS SDK (sysroot for `-lSystem`). Windows support is in progress — PAL threading, linker, and SDK discovery implemented; needs end-to-end testing (see `docs/windows-support.md`). Multi-epoch toolchain management (`promise sync`, shim) is described in `docs/module-system.md` Section 7.

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
curl -sSf https://promise-lang.dev/install.sh | sh -s -- --epoch 2026.0
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
    llvm/            ← LLVM tools extracted from the binary (Linux + macOS)
  lib/
    std/             ← embedded standard library source
    crt/             ← musl CRT objects (Linux only)
```

It then prints the PATH export line. Users add `~/.promise/bin` to their `PATH` once; all future `promise` invocations use the installed binary.

### 2.4 Updating

Re-running `promise install` with a newer binary replaces the installation in place. For epoch upgrades, `promise sync` (described in `docs/module-system.md` §7) downloads the correct binary for a given epoch and calls the equivalent of `promise install` on it. Until multi-epoch support is implemented, re-running the install script with `--epoch` is the update mechanism.

### 2.5 Windows

Windows support uses native MSVC ABI (`x86_64-pc-windows-msvc`). The compiler binary `promise.exe` is built on Windows and produces Windows executables by compiling LLVM IR through `opt` → `llc` → `lld-link`, linking against the Windows SDK and UCRT.

**Prerequisites:** Visual Studio Build Tools (provides MSVC libs + Windows SDK) and LLVM 22+. See `docs/windows-support.md` for full details.

**Build from source:**
```batch
build.bat
```

**Install:** `bin\promise.exe install` sets up `%USERPROFILE%\.promise\` with the binary and stdlib.

**Status:** PAL threading (CreateThread, CRITICAL_SECTION, CONDITION_VARIABLE), linker support (lld-link), and SDK discovery are implemented. Needs end-to-end testing on Windows.

---

## 3. Release Artifacts

Each release publishes to **GitHub Releases** at `github.com/promise-lang/promise`, tagged `epoch-YYYY.N`.

| Binary | Platform |
|--------|----------|
| `promise-linux-amd64` | Linux x86_64, fully static (musl). Implemented. |
| `promise-linux-arm64` | Linux ARM64. Planned. |
| `promise-darwin-amd64` | macOS Intel. Needs Xcode CLT. Implemented. |
| `promise-darwin-arm64` | macOS Apple Silicon. Needs Xcode CLT. Implemented. |
| `promise-windows-amd64.exe` | Windows x86_64. Needs VS Build Tools. In progress. |

Each release also includes a `SHA256SUMS` file. `scripts/install.sh` verifies the checksum before running `promise install`.

---

## 4. macOS Notes

The macOS release binary embeds LLVM tools (`opt`, `llc`, `lld`, `libLLVM.dylib`) using the same gzip + `go:embed` pattern as Linux. This eliminates the need for `brew install llvm`. However, the macOS SDK is still required for linking (provides `-lSystem`).

**Requirement:** Install Xcode Command Line Tools:

```sh
xcode-select --install
```

This provides the macOS SDK sysroot. The embedded `ld64.lld` (a symlink to the bundled `lld`) handles Mach-O linking — the system `ld` is not needed.

**Build a release binary on macOS:**

```sh
# Requires: brew install llvm (22+) — used at build time to bundle tools
./build --release
```

This runs `make llvm-bundle-darwin`, which finds Homebrew LLVM (and separately the `lld` formula), gzip-compresses `opt`, `llc`, `lld`, `libLLVM.dylib`, all `liblld*.dylib` libraries, and any transitive non-system Homebrew dependencies (e.g., `libz3`, `libzstd`), then builds with `-tags embed_llvm`. Platform-specific embed files (`llvm_darwin_arm64.go` / `llvm_darwin_amd64.go`) include the compressed tools via `go:embed`.

**Runtime extraction:** On first use, embedded tools are extracted to `~/.promise/cache/llvm/darwin-{arm64,amd64}/`. After extraction, Mach-O binaries are patched:
1. `install_name_tool -add_rpath @loader_path` — so tools find dylibs in their own directory
2. `install_name_tool -change` — rewrites absolute Homebrew paths (`/opt/homebrew/...`) to `@rpath/<name>`
3. `install_name_tool -id @rpath/<name>` — patches dylib install names
4. `codesign --force --sign -` — ad-hoc re-signing (required after Mach-O modification on macOS)

`DYLD_LIBRARY_PATH` is set when running extracted tools so they can find `libLLVM.dylib` and transitive dependencies (e.g., `libz3`, `libzstd`).

**Non-release builds** (dev builds without `--release`) continue to use Homebrew LLVM or system tools from PATH, same as before.

**Planned: zero-dependency macOS binary.** The current macOS release binary still requires Xcode Command Line Tools for the macOS SDK sysroot (needed by `ld64.lld` for `-lSystem`). A future improvement could bundle the minimal SDK surface — `libSystem.tbd` stubs and essential headers — directly in the binary, similar to how Go ships its own linker and doesn't require system tools. This would make the macOS binary fully self-contained: download and run with no prerequisites.

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
    curl -sSf https://promise-lang.dev/install.sh | sh -s -- --epoch 2026.0
    echo "$HOME/.promise/bin" >> $GITHUB_PATH
```

For Docker, copy the binary into the image directly — no install script needed:

```dockerfile
FROM ubuntu:24.04
COPY promise-linux-amd64 /usr/local/bin/promise
RUN promise install
```

`promise doctor` (planned — T0174) can be used in CI to verify the environment before running builds or tests:

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

Tags follow the format `epoch-YYYY.N` (e.g., `epoch-2026.0`). A tag on main is a release. Nothing else triggers a release.

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
- **Windows**: In progress. PAL, linker, and `promise install` are implemented. Add `windows-latest` to the CI matrix after end-to-end validation on a Windows machine. Requires VS Build Tools + LLVM 22+ on the runner.

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
      - name: Install LLVM
        run: brew install llvm
      # macOS release binary: embeds LLVM tools (opt, llc, lld, libLLVM.dylib).
      # Requires Xcode CLT at runtime for macOS SDK (sysroot).
      - name: Build (release)
        run: ./build --release
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
      - name: Install LLVM
        run: brew install llvm
      - name: Build (release)
        run: ./build --release
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
git tag epoch-2026.0
git push origin epoch-2026.0
```

That's it. The tag push triggers the release workflow, which builds all platform binaries and creates the GitHub Release automatically. No manual binary uploads, no manual SHA256 computation.

### 7.5 Planned Platform Additions

| Platform | Blocker | Notes |
|----------|---------|-------|
| `linux-arm64` | Cross-compile + arm64 runner | Go cross-compiles fine. Musl CRT arm64 objects needed. `embed_llvm` needs arm64 LLVM tools bundled. |
| `windows-amd64` | End-to-end testing on Windows | PAL threading, linker (lld-link), SDK discovery, `promise install` implemented. Needs testing on Windows machine, then add `build-windows-amd64` CI job on `windows-latest`. See `docs/windows-support.md`. |
| macOS zero-dep | Bundle macOS SDK stubs | Currently requires Xcode CLT for `-lSystem` sysroot. Could embed minimal SDK surface (`libSystem.tbd` + headers) like Go embeds its own linker. Would make macOS binary fully self-contained (download and run, no prerequisites). |
