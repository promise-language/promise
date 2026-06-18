# D0007: Epoch-Versioned Side-by-Side Installs

> **Updated by the distribution model.** Two parts of this plan are superseded by [distribution.md](distribution.md): (1) per-epoch `bin/llvm/` copies are replaced by a **shared content-addressed blob store** (`~/.promise/cache/blobs/`) that dedups dependencies across epochs and targets — epoch dirs reference blobs by hash rather than copying them ([distribution.md](distribution.md) §1.3, §4); (2) the Phase 2 *shim-in-binary* is the **current** implementation, but the **target** is a dedicated tiny **stub** (written in Promise, extracted at install, forward-only) — see [distribution.md](distribution.md) §2.5. The per-epoch dispatch *semantics* below remain correct; only the mechanism (shim-in-binary → extracted stub) and the dependency layout (per-epoch copy → shared blob store) change. Inline notes below mark the affected sections.

## Context

Promise uses a mono-versioned catalog system where each "epoch" (e.g., `2026.0`) is an atomic release of the entire platform: compiler + standard library + catalog modules + LLVM tools. The compiler binary is self-contained (~61MB), embedding everything via `go:embed`.

Currently, `promise install` creates a single flat installation at `~/.promise/`. There is no way to have multiple epochs installed simultaneously. Projects pin `epoch` in `promise.toml`, but epoch mismatches produce warnings, not dispatch to the correct compiler.

This plan implements the multi-epoch layout already designed in `docs/module-system.md` Section 7, balancing two audiences:
- **Platform developers** (compiler/stdlib/module authors) who iterate with `./build` and need escape hatches
- **Platform users** who `promise update`/`promise use` and expect automatic epoch dispatch per project

---

## Phase 1: Foundation — Epoch Directory Layout

**Goal**: `promise install` writes into `~/.promise/epochs/<epoch>/` instead of flat `~/.promise/`. Introduce the `active` file.

### Directory layout

> **Layout note (superseded).** The per-epoch `bin/llvm/` and `lib/crt/` copies shown below are replaced by the shared content-addressed blob store (`~/.promise/cache/blobs/sha256/<hash>`) — each epoch *references* the LLVM/CRT blobs it needs by hash instead of holding its own copy. The target layout is in [distribution.md](distribution.md) §2.4 / §4. The original per-epoch layout is retained here for historical context:

```
~/.promise/
  bin/promise               # stub (copy of binary, becomes shim in Phase 2; TARGET: tiny extracted stub)
  active                    # single line: "2026.0" (the default epoch)
  cache/
    blobs/sha256/<hash>     # TARGET: shared content-addressed dependency store (LLVM, CRT, wasm runner, sysroots)
  epochs/
    dev/                    # local dev builds (via bin/build + install)
      bin/promise
      bin/llvm/             # SUPERSEDED → referenced from cache/blobs/ by hash
      lib/std/
      lib/crt/              # SUPERSEDED → referenced from cache/blobs/ by hash
      lib/modules/
      cache/
        build/
        modules/
    2026.0/                 # release epoch (via promise update/use or promise install)
      bin/promise           # compiler binary for this epoch
      bin/llvm/             # SUPERSEDED → referenced from cache/blobs/ by hash
      lib/std/              # extracted standard library
      lib/crt/              # SUPERSEDED → referenced from cache/blobs/ by hash
      lib/modules/          # extracted catalog modules (io, json, etc.)
      cache/
        build/              # build cache for this epoch (compiled .o, test binaries)
        modules/            # git-fetched remote modules
    next/                   # pre-release epoch (via promise update on the next channel)
      ...                   # same layout as above
```

**All builds use the epoch layout** — dev, next, and stable all install into `epochs/<name>/` with identical structure. This eliminates the need for a separate shared cache codepath. The compiler always runs from an epoch directory and finds its cache, modules, and tools relative to its own epoch root.

**Per-epoch cache rationale**: Since cache keys include `compilerHash`, entries from different epochs never match. Keeping build cache inside the epoch directory means `promise remove <epoch>` is a single `rm -rf epochs/<epoch>/` with no orphaned entries. *(Holds for the per-epoch **build** cache, which stays in the epoch dir. The shared content-addressed **blob** store is separate and shared: removing an epoch additionally requires dropping its refs and GC'ing blobs referenced by no remaining epoch — see [distribution.md](distribution.md) §4 GC. `rm -rf` reclaims the epoch dir but not its now-unreferenced shared blobs.)*

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

> **Implemented as the dedicated stub (T0770).** The shim-in-binary design described below has been **removed**; `~/.promise/bin/promise` is now a tiny Promise-written stub ([tools/stub/main.pr](../tools/stub/main.pr)) that `exec`-replaces itself with the epoch compiler (same PID on Unix) and is forward-only updated at install via a `.promise-stub-version` sidecar. All trampoline logic (`shimDispatch`/`PROMISE_NO_SHIM`/`.promise.shim`) is gone from the compiler — invoking the compiler directly always runs the compiler, with an epoch mismatch surfaced as a *warning* (not a silent hand-off). See [distribution.md](distribution.md) §2.5/§2.6. The dispatch *semantics* in this section (epoch resolution order, single-file fallback) carry over to the stub; the historical shim-in-binary description below is retained only for context.

### Design: shim-in-binary (current)

The shim is **not** a separate program. It's a fast-path check at the top of `main()` in the existing compiler. This avoids maintaining a separate binary. *(Target: move this logic out into the dedicated Promise stub and remove `shimDispatch`/`PROMISE_NO_SHIM`/`.promise.shim` from the compiler entirely.)*

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
   b. No → print "epoch <desired> is not installed. Run: promise use <desired>" → exit 1
```

### Commands excluded from dispatch

These always run on the current binary (check `os.Args[1]` before dispatch):
- `install` — installing the current binary
- `update` / `use` — downloading and activating epochs
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

## Phase 3: `promise update` and the update channel (T0825)

**Goal**: Download compiler binaries from GitHub releases, decoupling *which release
stream you follow* from *which installed compiler runs builds*.

### Two orthogonal axes

The old `promise sync` conflated two independent concepts. They are now separately
persisted:

| Axis | What it controls | Selected by | File |
|---|---|---|---|
| **Active epoch** | Which installed compiler runs builds right now. | `promise use <epoch>` (+ project `promise.toml [module].epoch`) | `~/.promise/active` |
| **Update channel** | Which release stream `promise update` follows. | `promise update channel <name>` | `~/.promise/channel` (default `stable`) |

Channels are `stable` (the latest tagged `epoch-*` release) and `next` (the rolling
`epoch-next` pre-release). Default is `stable`. `sync` is **deleted** — installing a
specific/historical epoch is now `promise use <epoch>`, which downloads on demand.

### Command surface (`compiler/cmd/promise/update.go`)

```
promise update                        # follow the channel: install its latest + ACTIVATE
promise update check [--json]         # report availability; NO mutation
promise update channel                # print the current channel
promise update channel <stable|next>  # set the channel AND immediately follow it

promise use <epoch>                   # activate an epoch; DOWNLOAD from releases if missing
```

`update` **auto-activates** the freshly installed epoch (the child `install` writes
`~/.promise/active`) — otherwise a `promise use <epoch>` would freeze updates forever.

### Release channels

| Channel | What it is | Who uses it | Mutability |
|---------|-----------|-------------|------------|
| **Stable** (`channel stable`, the default) | Latest tagged GitHub release (`epoch-2026.0`). Immutable — same binary forever. | Platform users, CI, production. | Never changes once tagged. |
| **Next** (`channel next`) | The rolling `next` branch build. Pre-release GitHub release tagged `epoch-next`. | Module authors testing against the upcoming epoch. Early adopters. | Updated on every push to `next`. Re-running `promise update` on the next channel gets the latest build. |
| **Dev** (`bin/install.sh`) | Local build installed into `epochs/dev/`. | People working on the compiler itself. | Changes with every `bin/install.sh` run. |

**Coexistence**: All channels install into `epochs/` with the same layout. Quick
iteration with `./build && bin/promise ...` also works without installing — the
repo-local binary uses a shared cache fallback.

### Build identity for the rolling `next` channel

`stable` staleness is decided by comparing epoch tags numerically (the epoch *is* the
identity — see `module.CompareEpochs`, which splits `YYYY.N` and compares each half as
an integer so `2026.10` correctly ranks above `2026.9`). The `next` channel is rolling
and has no epoch identity, so `update check` needs a build identity.

**Chosen: the platform asset's SHA-256.** We already download `SHA256SUMS` to verify the
asset, so the platform asset's sha256 is free and is the exact identity of "the binary
I'd download." The commit hash is unreliable end-to-end (the release `target_commitish`
frequently returns a branch name, not a resolved SHA), so it is never the comparison key.

- **Persist**: after a successful release install, `downloadAndInstall` records the
  verified asset sha256 at `~/.promise/epochs/<epoch>/build-id`.
- **Check (next)**: fetch the `epoch-next` release's `SHA256SUMS`, read the platform
  line, and compare to the local `build-id`. Differ (or no local build-id) ⇒ update
  available.
- **Display**: any build id shown to a human is shortened to its first 7 hex chars
  (git-short-sha convention); the full 64-char hash is on-disk/JSON/comparison only.

### Version discovery

GitHub Releases API: `GET https://api.github.com/repos/<org>/<repo>/releases`, filtering
by the `epoch-*` tag prefix. Parse tag `epoch-2026.0` to get the epoch string. For
`next`, look for the release tagged `epoch-next` (pre-release flag set). The release
repository is configurable via `PROMISE_RELEASE_REPO` / `PROMISE_RELEASE_URL` so it can
point at the real repo or a corporate mirror.

### Download flow

1. Resolve the target release (channel `stable` → latest epoch tag; `next` → `epoch-next`; or the specific tag for `promise use <epoch>`)
2. Determine platform asset: `promise-<os>-<arch>[.exe].gz`
3. Download asset + `SHA256SUMS` from the release
4. Verify SHA256 over the `.gz` asset (what was downloaded)
5. Decompress and `chmod +x` the binary
6. Run `<downloaded-binary> install` → Phase 1 logic installs into `epochs/<epoch>/` and activates it
7. Record the verified asset sha256 as `epochs/<epoch>/build-id`
8. Print success: `epoch 2026.0 installed.`

### Progress

Print download progress to stderr: `downloading promise-darwin-arm64.gz... 45.2/61.0 MB`

### Error handling

`update` first prints the source being queried (once, to stderr) so failures
say *where* they looked: `Checking github.com/promise-language/promise for Promise releases (channel: stable)...`.

- Network failure → "cannot reach github.com/promise-language/promise (check your connection): ..."
- Non-2xx from the API → the request URL, the HTTP status, GitHub's own message, and an
  actionable hint keyed on the status: 404 → repo private / release missing (with
  `GITHUB_TOKEN` guidance), 401 → bad token, 403 → rate limit.
- Unknown epoch (404 on a specific tag) → "epoch 2026.1 is not available in
  github.com/promise-language/promise. Either the release tag does not exist, or the
  repository is private and you lack access." + `GITHUB_TOKEN` hint.
- Checksum mismatch → "checksum verification failed — download may be corrupted"
- Platform not available → "no binary available for linux-arm64 in epoch 2026.0"

**Private-repo / authenticated access**: API calls send `GITHUB_TOKEN` as a Bearer
token when set. When a token is present, asset downloads use the GitHub *API asset URL*
(`Accept: application/octet-stream`) rather than `browser_download_url` — the only way
to fetch assets from a private repository. The token is dropped on the cross-domain
redirect to the CDN, so it never leaks to `objects.githubusercontent.com`.

### Verification
- `promise use 2026.0` (when missing) downloads and installs into `epochs/2026.0/`
- `promise epochs` shows it
- `promise update channel next && promise update` installs the `epoch-next` build and records its `build-id`
- `promise update check` reports up-to-date / available without mutating anything

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
| Restoring official release after dev override | `promise use 2026.0` → re-downloads the release binary on demand |
| Module author testing against next epoch | `promise update channel next` → sets the channel and installs+activates the pre-release compiler |
| Multiple projects, different epochs | No action needed — shim reads each project's `promise.toml` epoch |

### How the three channels coexist

```
~/.promise/
  epochs/
    dev/bin/promise         # local dev build (via bin/install.sh)
    2026.2/bin/promise      # old stable (still installed for legacy project)
    2026.3/bin/promise      # current stable / latest next build (the next branch
                            #   carries a concrete YYYY.N — its install lands here)
    next/build-id           # next channel record only (NOT an install dir): the
                            #   sha256 `update check` compares; no bin/ here
  active                    # "dev" or "2026.3" (default for scripts without promise.toml)
  channel                   # "stable" (default) or "next" — what `update` follows
```

Following the `next` channel does NOT create an `epochs/next/` install — the
downloaded `epoch-next` binary's `install` activates the concrete `YYYY.N` epoch it
carries. `epochs/next/build-id` is just the channel's build identity for `update
check`. (This is why `promise use next` is rejected: `next` is a channel, not an
epoch — follow it with `promise update channel next`.)

A developer can:
1. Have projects pinned to `dev`, `2026.2`, or `2026.3` — the shim handles dispatch
2. Run `bin/install.sh` to push their local build into `epochs/dev/`
3. Run `promise use 2026.3` to switch back to a release epoch
4. Run `promise use 2026.3` to re-download the official release on demand if needed

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
- `promise remove <epoch>` is a clean `rm -rf epochs/<name>/` for the epoch's own dir (build cache included) — but the shared **blob** store needs the union-rooted GC step (drop refs, then sweep blobs no remaining epoch references; [distribution.md](distribution.md) §4 GC), not just `rm -rf`
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
| `compiler/cmd/promise/update.go` | **New** — runUpdate() + check/channel subverbs, GitHub release download, build-id persistence (T0825) |
| `compiler/cmd/promise/main.go` | Modify: runInstall() epoch layout, add shim call in main(), new commands (use, epochs, remove) |
| `compiler/internal/module/cache.go` | Minor: adjust extractEmbeddedModule() to check epoch-local lib first |
| `compiler/internal/module/catalog.go` | Unchanged (already has Epoch field) |
| `compiler/internal/module/config.go` | Unchanged (already parses epoch from promise.toml) |
| `docs/module-system.md` | Update Section 7 to match implementation |
| `docs/distribution.md` | Update installation/update docs |

---

## Open Questions for Discussion

1. **Should `promise update` set the new epoch as active automatically?** Resolved (T0825): yes — `update` auto-activates the freshly installed epoch, otherwise a `promise use <epoch>` would freeze updates forever. A specific epoch is selected explicitly with `promise use <epoch>`.

2. **Epoch mismatch behavior**: Currently a warning. With the shim, mismatches are auto-resolved. Should the compiler still warn when `PROMISE_EPOCH=local` overrides a project's pinned epoch?

3. **Minimum supported epochs**: Should there be a way to express "this project works with any epoch >= 2026.2"? Or is exact-match-only the right default for simplicity?

4. **GitHub release URL**: The plan hardcodes `github.com/promise-language/promise`. Should this be configurable for corporate mirrors / air-gapped environments?

5. **Remote module cache**: Git-fetched remote modules (`[require]` entries) live at `epochs/<epoch>/cache/modules/`. Should these be shared across epochs instead? Two epochs using the same remote module at the same commit would duplicate the git clone. On the other hand, per-epoch keeps removal simple.
