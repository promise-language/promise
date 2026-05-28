# D0007: Epoch-Versioned Side-by-Side Installs

## Context

Promise uses a mono-versioned catalog system where each "epoch" (e.g., `2026.0`) is an atomic release of the entire platform: compiler + standard library + catalog modules + LLVM tools. The compiler binary is self-contained (~61MB), embedding everything via `go:embed`.

Currently, `promise install` creates a single flat installation at `~/.promise/`. There is no way to have multiple epochs installed simultaneously. Projects pin `epoch` in `promise.toml`, but epoch mismatches produce warnings, not dispatch to the correct compiler.

This plan implements the multi-epoch layout already designed in `docs/module-system.md` Section 7, balancing two audiences:
- **Platform developers** (compiler/stdlib/module authors) who iterate with `./build` and need escape hatches
- **Platform users** who `promise sync` and expect automatic epoch dispatch per project

---

## Phase 1: Foundation — Epoch Directory Layout

**Goal**: `promise install` writes into `~/.promise/epochs/<epoch>/` instead of flat `~/.promise/`. Introduce the `active` file.

### Directory layout

```
~/.promise/
  bin/promise               # stub (copy of binary, becomes shim in Phase 2)
  active                    # single line: "2026.0" (the default epoch)
  epochs/
    dev/                    # local dev builds (via ./build + bin/install.sh)
      bin/promise
      bin/llvm/
      lib/std/
      lib/crt/
      lib/modules/
      cache/
        build/
        modules/
    2026.0/                 # release epoch (via promise sync or promise install)
      bin/promise           # compiler binary for this epoch
      bin/llvm/             # extracted LLVM tools (release builds)
      lib/std/              # extracted standard library
      lib/crt/              # musl CRT (Linux)
      lib/modules/          # extracted catalog modules (io, json, etc.)
      cache/
        build/              # build cache for this epoch (compiled .o, test binaries)
        modules/            # git-fetched remote modules
    next/                   # pre-release epoch (via promise sync next)
      ...                   # same layout as above
```

**All builds use the epoch layout** — dev, next, and stable all install into `epochs/<name>/` with identical structure. This eliminates the need for a separate shared cache codepath. The compiler always runs from an epoch directory and finds its cache, modules, and tools relative to its own epoch root.

**Per-epoch cache rationale**: Since cache keys include `compilerHash`, entries from different epochs never match. Keeping build cache inside the epoch directory means `promise remove <epoch>` is a single `rm -rf epochs/<epoch>/` with no orphaned entries.

### New file: `compiler/internal/module/epoch.go`

```go
func CompilerEpoch(catalogData []byte) string   // parse embedded catalog → epoch
func EpochDir(epoch string) (string, error)      // → <PromiseHome>/epochs/<epoch>/
func ActiveEpoch() (string, error)               // read <PromiseHome>/active, fallback: latest in epochs/
func WriteActiveEpoch(epoch string) error         // write active file
func InstalledEpochs() ([]string, error)          // list dirs under epochs/
```

### Changes to `runInstall()` in `main.go`

1. Parse embedded `catalog.toml` to get epoch string
2. Compute `epochDir = ~/.promise/epochs/<epoch>/`
3. Install: binary → `epochDir/bin/promise`, std → `epochDir/lib/std/`, LLVM → `epochDir/bin/llvm/`, CRT → `epochDir/lib/crt/`, catalog modules → `epochDir/lib/modules/<name>/`
4. Copy binary to `~/.promise/bin/promise` (stub shim for Phase 1)
5. Write `~/.promise/active` with current epoch
6. Print PATH instructions (unchanged)

### Migration

- Old `~/.promise/bin/promise` is overwritten with the new stub
- Old `~/.promise/lib/std/` etc. are left in place (harmless, cleaned up later)
- Users with `~/.promise/bin` in PATH need zero changes
- `promise clean --global` can optionally remove legacy flat files

### LLVM/CRT tool discovery

No changes needed — the existing "sibling of binary" search (`filepath.Dir(os.Executable())`) resolves to `epochs/<epoch>/bin/` which already contains `llvm/` after install.

### Cache layout

- All epochs (dev, stable, next) use per-epoch cache at `epochs/<name>/cache/build/`.
- The compiler detects its epoch directory from its own binary path (`os.Executable()` → walk up to find `epochs/<name>/`).
- Each epoch uses its own `lib/modules/` for catalog modules — no shared cache, no stamp-based invalidation needed.

### Verification
- `promise install` produces the epoch layout
- `~/.promise/epochs/2026.0/bin/promise build file.pr` works
- `~/.promise/bin/promise build file.pr` works (stub = same binary)
- All existing tests pass unchanged

---

## Phase 2: The Shim

**Goal**: Replace the stub at `~/.promise/bin/promise` with dispatch logic that reads the project epoch and delegates to the correct epoch's compiler.

### Design: shim-in-binary

The shim is **not** a separate program. It's a fast-path check at the top of `main()` in the existing compiler. This avoids maintaining a separate binary.

### New file: `compiler/cmd/promise/shim.go`

`shimDispatch()` — called at the very top of `main()`, before command parsing:

```
1. If PROMISE_NO_SHIM=1, return (prevents infinite recursion)
2. Determine desired epoch:
   a. PROMISE_EPOCH env var → use it (developer override)
   b. FindConfig(cwd) → read [module].epoch
   c. ActiveEpoch() fallback
3. Determine my epoch: CompilerEpoch(embeddedCatalog)
4. If desired == my epoch → return (proceed normally)
5. Check ~/.promise/epochs/<desired>/bin/promise exists
   a. Yes → syscall.Exec into it with PROMISE_NO_SHIM=1
   b. No → print "epoch <desired> is not installed. Run: promise sync <desired>" → exit 1
```

### Commands excluded from dispatch

These always run on the current binary (check `os.Args[1]` before dispatch):
- `install` — installing the current binary
- `sync` — downloading another epoch
- `epochs` / `use` — managing epochs
- `init` — creating new project (uses current epoch)

### New commands

| Command | Description |
|---------|-------------|
| `promise use <epoch>` | Set default epoch (`~/.promise/active`). Validates epoch is installed. |
| `promise epochs` | List installed epochs, mark active with `*`, show disk usage. |

### Single-file mode

When no `promise.toml` is found, falls through to `ActiveEpoch()`. This preserves `promise run script.pr` behavior.

### Platform exec

- Unix: `syscall.Exec` (replaces process — no child, no signal forwarding needed)
- Windows: `os.StartProcess` + wait + exit with child's exit code

### Verification
- Install epoch A and B into `epochs/`
- Project with `epoch = "A"` → shim dispatches to A
- Project with `epoch = "B"` → shim dispatches to B
- No `promise.toml` → uses `active` epoch
- Missing epoch → clear error message

---

## Phase 3: `promise sync`

**Goal**: Download compiler binaries for target epochs from GitHub releases.

### New file: `compiler/cmd/promise/sync.go`

```
promise sync                  # latest stable epoch (tagged release)
promise sync 2026.0           # specific stable epoch
promise sync next             # latest pre-release build (next branch)
```

### Release channels

| Channel | What it is | Who uses it | Mutability |
|---------|-----------|-------------|------------|
| **Stable** (`promise sync` / `promise sync 2026.0`) | Tagged GitHub release (`epoch-2026.0`). Immutable — same binary forever. | Platform users, CI, production. | Never changes once tagged. |
| **Next** (`promise sync next`) | Latest build from the `next` branch. Pre-release GitHub release tagged `epoch-next`. | Module authors testing against upcoming epoch. Early adopters. | Updated on every push to `next`. Re-running `promise sync next` gets the latest build. |
| **Dev** (`bin/install.sh`) | Local build installed into `epochs/dev/`. | People working on the compiler itself. | Changes with every `bin/install.sh` run. |

**Coexistence**: All three channels install into `epochs/` with the same layout. Dev builds go to `epochs/dev/`, next to `epochs/next/`, stable to `epochs/2026.0/`. Quick iteration with `./build && bin/promise ...` also works without installing — the repo-local binary uses a shared cache fallback.

**Lifecycle of an epoch**:
```
Local dev (./build) → push to next branch → promise sync next → tag epoch-2026.4 → promise sync 2026.4
```

When `2026.4` is tagged, `promise sync next` starts tracking the *next* upcoming epoch. Users on `next` who want stability switch to `promise use 2026.4`.

### Version discovery

Use GitHub Releases API once the repository is published. The URL pattern will be `GET https://api.github.com/repos/<org>/<repo>/releases`, filtering by `epoch-*` tag prefix. Parse tag `epoch-2026.0` to get epoch string. For `next`, look for the release tagged `epoch-next` (pre-release flag set).

The release repository URL should be configurable — stored in the embedded catalog or a separate config — so it can point to the actual repo once created, or to corporate mirrors.

### Download flow

1. Resolve target epoch (latest tag = stable, pre-release = next, or specific)
2. Check if already installed → skip
3. Determine platform binary: `promise-<os>-<arch>` (e.g., `promise-darwin-arm64`)
4. Download binary + `SHA256SUMS` from the release
5. Verify SHA256 checksum
6. `chmod +x` the downloaded binary
7. Run `<downloaded-binary> install` → Phase 1 logic installs into `epochs/<epoch>/`
8. Print success: `epoch 2026.0 installed. Active epoch: 2026.0`

### Progress

Print download progress to stderr: `downloading promise-darwin-arm64... 45.2/61.0 MB`

### Error handling

- Network failure → "cannot reach GitHub releases. Check your connection."
- Unknown epoch → "epoch 2026.1 is not available. Run `promise sync` for latest."
- Checksum mismatch → "checksum verification failed — download may be corrupted"
- Platform not available → "no binary available for linux-arm64 in epoch 2026.0"

### Verification
- `promise sync 2026.0` downloads and installs into `epochs/2026.0/`
- `promise epochs` shows it
- `promise sync 2026.0` again → "already installed"

---

## Phase 4: Developer Workflow

**Goal**: `./build` and `bin/install.sh` work seamlessly alongside the epoch system.

### Core principle

**Dev builds install into `epochs/dev/`.** Running `./build` produces `bin/promise` at the repo root (unchanged). Running `bin/install.sh` (or `bin/promise install`) installs into `~/.promise/epochs/dev/` with the same layout as release epochs. The `dev` epoch name is special — not a valid `YYYY.N` pattern, so it never collides with release epochs.

### `bin/install.sh` changes

Currently `bin/install.sh` does `./build --release && bin/promise install`. With epochs:

1. `./build --release` → produces `bin/promise` (unchanged)
2. `bin/promise install` → detects the embedded epoch from `catalog.toml`, but since this is a dev build, installs into `epochs/dev/` instead of `epochs/<epoch>/`
3. Sets `active` to `dev` (so the shim uses it by default)

**Detection**: `promise install` checks whether the binary was built with `--release` by the presence/absence of a build flag or embedded marker. Alternatively, `promise install --dev` explicitly targets `epochs/dev/`, and `bin/install.sh` passes this flag. The simpler approach: `bin/install.sh` always passes `--dev`, end-user `promise install` (from a release binary) does not.

### `PROMISE_EPOCH` env var

The developer escape hatch. Values:

| Value | Effect |
|-------|--------|
| `dev` | Force dispatch to the dev epoch |
| `2026.0` | Force dispatch to that epoch |
| `/path/to/bin/promise` | Exec that specific binary directly |

### Developer scenarios

| Scenario | How it works |
|----------|-------------|
| Working on the compiler (quick iteration) | `./build && bin/promise test tests/...` — direct invocation, no shim, uses shared cache |
| Installing dev build for project testing | `bin/install.sh` → installs into `epochs/dev/`, sets active to `dev` |
| Testing dev build against epoch-pinned project | `cd ~/myproject && PROMISE_EPOCH=dev promise build main.pr` |
| Switching back to released epoch | `promise use 2026.0` → changes active epoch |
| Restoring official release after dev override | `promise sync 2026.0` → re-downloads the release binary |
| Module author testing against next epoch | `promise sync next && promise use next` → uses pre-release compiler |
| Multiple projects, different epochs | No action needed — shim reads each project's `promise.toml` epoch |

### How the three channels coexist

```
~/.promise/
  epochs/
    dev/bin/promise         # local dev build (via bin/install.sh)
    2026.2/bin/promise      # old stable (still installed for legacy project)
    2026.3/bin/promise      # current stable
    next/bin/promise        # pre-release (updated by `promise sync next`)
  active                    # "dev" or "2026.0" (default for scripts without promise.toml)
```

A developer can:
1. Have projects pinned to `dev`, `2026.2`, `2026.3`, or `next` — the shim handles dispatch
2. Run `bin/install.sh` to push their local build into `epochs/dev/`
3. Run `promise use 2026.3` to switch back to a release epoch
4. Run `promise sync 2026.3` to re-download the official release if needed

### `./build` itself is unchanged

Build script continues to output `bin/promise` at the repo root. Running `bin/promise` directly (without the shim) still works for quick iteration — it just uses the shared `~/.promise/cache/` instead of per-epoch cache. For full epoch-integrated testing, use `bin/install.sh`.

### Verification
- `bin/install.sh` installs into `epochs/dev/`
- `promise epochs` shows `dev` alongside release epochs
- `PROMISE_EPOCH=dev` dispatches to the dev build
- Running `bin/promise` directly works for quick iteration

---

## Phase 5: Cleanup and Management

**Goal**: Manage disk space and prevent cache thrashing across epochs.

### Epoch removal

```
promise remove <epoch>                # delete epochs/<epoch>/ (refuses active without --force)
promise remove --all-except-active    # remove all epochs except the active one
promise clean --epochs                # same as remove --all-except-active
```

### Catalog module extraction

All epochs (dev, stable, next) have modules pre-extracted at `epochs/<name>/lib/modules/<name>/` during install. The compiler finds its epoch root from `os.Executable()` (walk up to `epochs/<name>/`) and resolves modules and cache relative to it.

For repo-local `bin/promise` (quick iteration without install), the compiler falls back to extracting modules to `~/.promise/cache/embedded_modules/` as it does today.

### Disk space reporting

`promise epochs` output:

```
  2026.2    67 MB   (12 MB cache)
* 2026.0    67 MB   (45 MB cache)   (active)

2 epochs, 134 MB + 57 MB cache
```

### Per-epoch build cache

Each epoch's build cache lives at `epochs/<name>/cache/build/`. This means:
- `promise remove <epoch>` is a clean `rm -rf epochs/<name>/` — no orphaned cache entries anywhere
- No need to scan/filter shared cache when cleaning up an epoch
- Repo-local `bin/promise` (quick iteration) uses `~/.promise/cache/build/` as fallback

### Verification
- `promise remove 2026.2` deletes the directory
- `promise remove 2026.3` (active) → error unless `--force`
- `promise epochs` shows correct disk usage

---

## Implementation Order

```
Phase 1 (Foundation)    ← start here, no dependencies
    ↓
Phase 2 (Shim)          ← depends on Phase 1
    ↓
Phase 3 (Sync)    ←──── depends on Phase 2
Phase 4 (Dev Workflow)   ← depends on Phase 2 (can parallelize with Phase 3)
    ↓
Phase 5 (Cleanup)        ← depends on Phase 1+2
```

Each phase is independently shippable. Phase 1 alone is useful (cleaner layout). Phase 2 adds the key user-facing feature. Phases 3-5 are polish.

---

## Critical Files

| File | Changes |
|------|---------|
| `compiler/internal/module/epoch.go` | **New** — CompilerEpoch, EpochDir, ActiveEpoch, InstalledEpochs |
| `compiler/internal/module/home.go` | Unchanged — PromiseHome() still returns `~/.promise/` |
| `compiler/cmd/promise/shim.go` | **New** — shimDispatch() logic |
| `compiler/cmd/promise/sync.go` | **New** — runSync(), GitHub release download |
| `compiler/cmd/promise/main.go` | Modify: runInstall() epoch layout, add shim call in main(), new commands (use, epochs, remove) |
| `compiler/internal/module/cache.go` | Minor: adjust extractEmbeddedModule() to check epoch-local lib first |
| `compiler/internal/module/catalog.go` | Unchanged (already has Epoch field) |
| `compiler/internal/module/config.go` | Unchanged (already parses epoch from promise.toml) |
| `docs/module-system.md` | Update Section 7 to match implementation |
| `docs/distribution.md` | Update installation/update docs |

---

## Open Questions for Discussion

1. **Should `promise sync` set the new epoch as active automatically?** The plan assumes yes (latest synced = active). Alternative: always require explicit `promise use <epoch>`.

2. **Epoch mismatch behavior**: Currently a warning. With the shim, mismatches are auto-resolved. Should the compiler still warn when `PROMISE_EPOCH=local` overrides a project's pinned epoch?

3. **Minimum supported epochs**: Should there be a way to express "this project works with any epoch >= 2026.2"? Or is exact-match-only the right default for simplicity?

4. **GitHub release URL**: The plan hardcodes `github.com/promise-language/promise`. Should this be configurable for corporate mirrors / air-gapped environments?

5. **Remote module cache**: Git-fetched remote modules (`[require]` entries) live at `epochs/<epoch>/cache/modules/`. Should these be shared across epochs instead? Two epochs using the same remote module at the same commit would duplicate the git clone. On the other hand, per-epoch keeps removal simple.
