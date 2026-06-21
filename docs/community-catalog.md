# The Community Catalog

This document specifies the **community module tier** (`docs/module-system.md` §9.9–§9.10): a single, decentralized git registry — `github.com/promise-community/catalog` — that makes community modules *name-addressable* and records their *per-epoch compatibility*, plus the CI that keeps that record accurate and the module-owner workflow for getting listed.

It builds directly on the epoch-aware resolution + verification engine (epoch-tag walk-back, the empirical "compiles + 100% of `` `test `` functions pass" gate, and the §9.10 no-compatible-version error) — that engine is the single source of truth for "compatible"; the community tier only adds a registry, its CI, and the name-tier integration.

---

## 1. Why a separate, living repo

The embedded first-party catalog (`catalog.toml`) is **frozen in the compiler binary** — one per epoch. That is correct for modules the language team owns and updates atomically (§4.4), but it cannot record a verdict that is only discovered *after* an epoch ships.

The community catalog is the opposite by design: a **living** git repo, fetched and cached on demand. A module that only achieves `epoch-2026.1` compatibility *after* `2026.1` shipped can be recorded there and become resolvable **without a compiler update**. That post-release recordability is the whole point of keeping it out of the binary.

The repo URL is a well-known constant (`https://github.com/promise-community/catalog`), overridable with the `PROMISE_COMMUNITY_CATALOG` environment variable for mirrors and air-gapped environments (§17).

---

## 2. Repository format

The catalog repo carries three payloads:

### 2.1 `modules.toml` — the name → URL map

Makes community modules name-addressable (`promise package add foo`, not a full URL):

```toml
[modules.foo]
url = "https://github.com/promise-community/foo"
description = "A foo library"

[modules.bar]
url = "https://github.com/promise-community/bar"
description = "Bar utilities"
```

Each entry **must** have a `url`. There is no `commit` and no `epoch` here — versioning is per-epoch, recorded in the compatibility index, not in this map.

### 2.2 `index/<epoch>.json` — the per-epoch compatibility index

**One file per epoch.** Each records, for every module verified for that epoch, the last-known-good commit its CI verified to compile + pass tests under that epoch:

```json
{
  "epoch": "2026.1",
  "modules": {
    "foo": {
      "commit": "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
      "tag": "epoch-2026.1",
      "verified_at": "2026-06-21T12:00:00Z",
      "compiler_hash": "…"
    }
  }
}
```

A module **absent** from `index/<E>.json` means *no verified version for epoch E* — the §9.10 signal.

Per-epoch files (rather than one large file) are deliberate: they keep the repo **append-only across epochs** with clean diffs, and let a post-release verdict be added for one epoch without rewriting the records of any other.

### 2.3 `matrix.md` — the published module × epoch grid

A CI-regenerated, human-readable view derived from all `index/*.json` files (✓ = verified for that epoch; — = no verified version). The index files are the single source of truth; `matrix.md` is a derived artifact — never hand-edit it.

---

## 3. Tiers and name resolution (client side)

A module's **tier is determined by its URL** — there is no separate flag:

| Tier | URL | Compatibility established by |
|------|-----|------------------------------|
| Embedded / first-party | no URL, or `github.com/promise-language/*` | epoch release gate (§4.2) — always 100% |
| Community | `github.com/promise-community/*` (listed in the community catalog) | the catalog's CI index, recorded per epoch |
| Ad-hoc remote | any other git URL | the project's own `promise test`, run locally on add |

`promise package add name` / `use name` resolves names in this order (§9.9):

1. `[replace]` (local override, §9.7)
2. `[require.NAME]` (project alias, §5.2)
3. embedded first-party catalog (§3.2) — no pin needed
4. **community catalog** (`github.com/promise-community/catalog`) — `add` resolves + pins
5. otherwise: not name-addressable — must be an explicit `promise package add <url> [ref]`

The embedded catalog **shadows** the community catalog on a name collision; an explicit URL always disambiguates.

**Add flow for a community module.** `promise package add foo` on a project at epoch E fetches (and refreshes) the catalog, looks `foo` up in `modules.toml`, reads `index/E.json`, and:

- **verified entry present** → pins that commit in `[require]` and reports it. **No local test run happens** — for community modules the CI index *is* the verdict (§9.9).
- **no entry for E** → emits the §9.10 "no version compatible with epoch E" error, naming the highest epoch the module *is* recorded for.

This is the key difference from the ad-hoc tier, where `promise` runs the module's tests locally on add and caches the verdict privately. The community CI run removes that per-user local run for listed modules.

**Update flow.** `promise package update` re-resolves a `[require]` entry whose URL is `github.com/promise-community/*` through the **fresh** index (authoritative, no local run) rather than the generic epoch-tag walk-back. Non-community entries keep the engine path.

---

## 4. Catalog CI — the compatibility matrix

The CI is a thin wrapper around one client-facing command, so "compatible" is defined in exactly one place:

```
promise package build-index <catalog-dir> <epoch> [-report]
```

For every module in `<catalog-dir>/modules.toml`, it resolves + verifies the epoch-appropriate commit under the **epoch-E compiler** (the same epoch-tag walk-back + empirical gate the client uses), then:

- writes the verified commits into `index/<epoch>.json`,
- regenerates `matrix.md`,
- exits non-zero if any **listed** module failed (so CI surfaces it) — except with `-report`, which prints the unsupported list and exits 0.

This is the authoritative per-epoch compat record for the community tier.

### Pre-release nudge (§9.10)

Before an epoch ships, running `build-index . <next-epoch> -report` with the upcoming epoch's compiler produces the "these listed modules don't support `<next>` yet" report — the soft analog of the first-party atomic coordination (§4.4). It cannot force an unmaintained module to update; that residual is the accepted cost of decentralization.

---

## 5. Module-owner workflow

### 5.1 Getting listed

Open a PR against `github.com/promise-community/catalog` adding a `[modules.NAME]` entry to `modules.toml`:

```toml
[modules.mything]
url = "https://github.com/you/mything"
description = "What it does"
```

Once merged, the catalog CI begins verifying your module each epoch and recording the verdict; users can then `promise package add mything`.

### 5.2 The per-epoch verify + tag loop

To make your module usable on a new epoch E:

```
promise use E        # switch to the epoch-E toolchain
promise test         # compile + run your `test` functions under E
git tag epoch-E      # on success, publish the tag
git push --tags
```

For listed modules the catalog CI does this across all of them automatically. The helper command bundles the verify step so an owner can self-check before tagging:

```
promise package check-epoch [<E>]
```

It verifies the module in the current directory against E (default: this compiler's epoch). On success it prints the publish hint (`git tag epoch-<E> && git push --tags`); on failure it surfaces the compact failure. Owners can also drop the `check-epoch.yml` CI template (below) into their own repo to self-verify against the next epoch.

### 5.3 Dependencies are transitive

A community module **may have its own dependencies**, but its compatibility with E is **transitive**: if any dependency has no E-compatible version, the module itself is incompatible with E (§9.10 applies to it too). The verification engine enforces this automatically — a failing transitive dep surfaces as a clean gate, not a raw compiler error.

---

## 6. CI workflow templates

The repo seed under `community-catalog/.github/workflows/` carries three templates:

- **`compat.yml`** — on push / scheduled: install the `stable` toolchain and run `promise package build-index . <stable-epoch>`, committing the updated `index/*.json` + `matrix.md` back. The authoritative per-epoch record.
- **`prerelease.yml`** — when a `next` epoch is cut: `promise install next` → `promise package build-index . <next-epoch> -report`, posting the unsupported-module list (the §9.10 nudge).
- **`check-epoch.yml`** — a **module-owner** template (for an author's own repo): `promise package check-epoch <E>`, then `git tag epoch-E && git push --tags` on success.

---

## 7. Ops: standing up the repo (one-time)

Creating the actual `github.com/promise-community/catalog` repo and enabling its Actions is a **one-time operational step**, separate from the compiler code. To bootstrap it:

1. Create the public repo `github.com/promise-community/catalog`.
2. Seed it from `community-catalog/` in this repository (`modules.toml`, empty `index/`, `README.md`, `.github/workflows/`).
3. Enable GitHub Actions and grant the workflow write access (it commits `index/*.json` + `matrix.md`).
4. Configure the schedule in `compat.yml` and the `next`-epoch trigger in `prerelease.yml`.

Everything that is *code* — the format, the client integration, the CI/owner tooling, and the workflow templates — lives in this repository. Only the hosted repo + Actions enablement is ops.
