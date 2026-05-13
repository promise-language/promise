# Phase 3: Remote Module Fetching via Git

Implementation plan for Stage 9 Phase 3 — remote module fetching.

## Architecture

Once a remote module is checked out to a local directory, it's indistinguishable from a local module. The design adds:

1. **Resolution layer**: URL + commit → local directory path
2. **Fetching layer**: ensure that directory exists in the global cache

The existing `moduleLoader.load()` handles everything after directory resolution (parsing, sema, cycle detection, caching).

## Global Cache Structure

```
~/.promise/cache/modules/
  github.com/someone/promise-parser/
    repo.git/              # bare git repo (shared across commits)
    a1b2c3d4e5f6/          # worktree checkout at specific commit (first 12 chars of SHA)
      promise.toml
      *.pr
    def5678abc12/
      ...
```

**Bare repo + worktree checkout**: `git clone --bare` once per repo, `git fetch` for new commits, `git archive | tar x` to populate per-commit directories (falls back to `--work-tree` checkout).

## Implementation Steps

### Step 1: `NormalizeURL()` — `module/config.go` [DONE]

Pure function to canonicalize remote module URLs for comparison:
- Strip schemes: `https://`, `http://`, `git://`, `ssh://`
- Strip trailing `.git`
- Strip trailing slashes
- Lowercase the host portion (preserves path case)
- Idempotent: `NormalizeURL(NormalizeURL(x)) == NormalizeURL(x)`

### Step 2: Global cache + git operations — `module/git.go` [DONE]

- `GlobalCacheDir() (string, error)` → `~/.promise/cache/modules/`
- `URLToCachePath(cacheDir, url) string` — normalized URL to filesystem path
- `ensureBareRepo(repoDir, url) error` — `git clone --bare` or `git fetch`
- `ensureCheckout(repoDir, checkoutDir, commitHash) error` — `git archive | tar x` (fallback: `--work-tree` checkout)
- `ResolveRemoteModule(url, commitHash) (string, error)` — orchestrates the above with fast-path for existing checkouts
- `PinResolve(url, ref) (string, error)` — resolve tag/branch/HEAD to full commit SHA via `git ls-remote`
- `IsFullCommitHash(s)` / `IsCommitHashLike(s)` — validation helpers
- Filesystem `.lock` file via `O_CREATE|O_EXCL` to prevent concurrent clone races; stale locks (>5 min) auto-cleaned
- `CleanGlobalCache()` — removes entire `~/.promise/cache/modules/`

### Step 3: `[replace]` resolution + `moduleLoader` integration [DONE]

Extended `moduleLoader` with:
- `projectCfg *module.Config` — root project's parsed promise.toml
- `remoteResolved map[string]string` — normalized URL → resolved absDir (dedup)
- `commitPins map[string]string` — effective commit pins after merging transitive [require]

`loadRemote(remoteURL, alias) (*sema.ModuleInfo, error)`:
1. Normalize URL
2. Check dedup cache (`remoteResolved`)
3. Check `[replace]` in root config → delegate to local `load()` (URL normalization applied for scheme-variant matching)
4. Look up commit pin → error with hint if missing
5. `ResolveRemoteModule(url, commit)` → absDir
6. Delegate to existing `load(absDir)` (accepts both relative and absolute paths)
7. Merge transitive `[require]` pins (top-level wins, conflict → error)

Both `loadModuleScopes()` and `loadDeps()` now dispatch non-local imports to `loadRemote()`.

### Step 4: Transitive pin resolution [DONE — integrated into Step 3]

When loading a remote module, its `promise.toml` [require] entries merge into effective pins:
- Top-level project pins always win (override)
- Conflicting pins from different modules → error with conflict chain
- Same pin → share the checkout

### Step 5: `promise pin` CLI command [DONE]

```
promise pin "github.com/someone/parser"          # pin to HEAD
promise pin "github.com/someone/parser" v2.1.0   # resolve tag
promise pin "github.com/someone/parser" a1b2c3d  # exact commit
```

Uses `PinResolve()` (via `git ls-remote`) to resolve refs. Writes full 40-char SHA to `promise.toml` `[require]` section via `SetRequire()`. Creates `[require]` section if missing; updates existing entries in-place (matched by normalized URL).

### Step 6: Epoch mismatch warnings [DONE]

When loading a module, compares its `epoch` field against the project's `epoch`. Emits a warning if they differ: `warning: module "foo" has epoch 2025.1, but project uses epoch 2026.0`. Warnings are collected in `moduleLoader.warnings` and printed to stderr after all modules are loaded.

### Step 7: `promise clean --global` [DONE]

`promise clean` removes the build cache (`~/.promise/cache/build/`). `promise clean --global` additionally removes the module cache (`~/.promise/cache/modules/`) via `CleanGlobalCache()`. All paths respect the `PROMISE_HOME` env var.

## Files

### New
| File | Purpose |
|------|---------|
| `module/git.go` | Git operations: bare clone/fetch, archive checkout, global cache, filesystem locking |
| `module/git_test.go` | Tests using local git repos in `t.TempDir()` (no network) |

### Modified
| File | Changes |
|------|---------|
| `module/config.go` | `NormalizeURL()`, `SetRequire()` (TOML write), `IsLocalPath()`, `IsCommitHashLike()` |
| `module/config_test.go` | URL normalization tests, `SetRequire` tests (new section, existing section, append, normalized match, preserve comments) |
| `cmd/promise/main.go` | `loadRemote()`, `runPin()`, `runClean(--global)`, epoch warnings via `ml.warnings`, `projectCfg`/`remoteResolved`/`commitPins` fields |
| `cmd/promise/parse_test.go` | Remote module tests (replace, scheme variant, no pin, nil config, pin conflict, top-level override), epoch mismatch/match tests |

### Unchanged
Grammar, AST, sema, codegen, `module/cache.go` — all module-agnostic.

## Design Decisions

- **Only top-level `[replace]` takes effect.** Transitive modules' `[replace]` is ignored.
- **Full 40-char commit SHA required.** `promise pin` always writes full hashes; short hashes accepted in [require] but warned if <12 chars.
- **Remote modules at pinned commits are immutable.** Source never changes → impl hash is stable → cached .o valid indefinitely.
- **`git` must be on PATH.** Clear error: "git is required for remote modules but was not found on PATH".
- **Archive-first checkout.** `git archive | tar x` is cleaner than `--work-tree` for bare repos; fallback to `--work-tree` if archive fails.
- **URL normalization is scheme-agnostic.** `https://github.com/x/y.git` and `github.com/x/y` and `ssh://git@github.com/x/y` all resolve to the same normalized form `git@github.com/x/y` or `github.com/x/y`.

## Risk Mitigations

| Risk | Mitigation |
|------|-----------|
| Git not installed | `exec.LookPath("git")` check before any git operation |
| Network failures | Propagate git's stderr directly — it has good error messages |
| Large repos | Future: `--filter=blob:none` partial clone, fallback to full |
| Short hash ambiguity | `promise pin` always resolves and writes full 40-char SHA |
| Concurrent cache access | `.lock` file with `O_CREATE\|O_EXCL`, 30s timeout, 5-min stale auto-cleanup |
| Cache growth | `promise clean --global` (Step 7), `CleanGlobalCache()` already implemented |
