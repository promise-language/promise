# promise-community/catalog

This directory is the **seed** for the community module catalog repo,
`github.com/promise-community/catalog` — the decentralized registry that makes
community Promise modules name-addressable and records their per-epoch
compatibility (`docs/community-catalog.md`, module-system.md §9.9–§9.10).

It is committed here so the client parser, the catalog CI, and the tests all
share one source of truth for the format. Standing up the real hosted repo +
enabling its Actions is a one-time ops step (see §7 of `docs/community-catalog.md`).

## Layout

- `modules.toml` — the `name → URL` map. One `[modules.NAME]` entry per listed
  module (each with a `url`). Getting a module listed = a PR adding an entry here.
- `index/<epoch>.json` — the per-epoch compatibility index, written by CI: for
  each listed module verified for that epoch, the last-known-good commit. One file
  per epoch (append-only, clean diffs). A module absent from an epoch's index has
  no verified version for it (§9.10).
- `matrix.md` — CI-regenerated module × epoch grid (✓ / —). Derived from
  `index/*.json`; never hand-edit.
- `.github/workflows/` — CI templates: `compat.yml` (per-epoch matrix build),
  `prerelease.yml` (the §9.10 pre-release nudge), and `check-epoch.yml` (a
  module-owner self-verify template to copy into your own repo).

## Maintained by CI

The `index/*.json` files and `matrix.md` are produced by
`promise package build-index . <epoch>` running in CI — do not edit them by hand.
Only `modules.toml` is edited directly (via PR).
