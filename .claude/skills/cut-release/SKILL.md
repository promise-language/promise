---
name: cut-release
description: Cut a stable Promise epoch release end-to-end — drive pinned all-platform CI, cut next, wait for the epoch-next release build, author synthesized "what changed" notes, cut stable, verify publish, and write a narrative summary for the user.
---

Cut a stable `epoch-<year>.<n>` release from a chosen commit. `bin/release` enforces the gates; this skill is the orchestration playbook around it, including the human-facing parts the tooling does NOT do (synthesized notes + a narrative write-up).

**Pick the release commit first.** Default to the current `origin/main` tip. It must be reachable from `origin/main` and must pass CI on ALL required platforms — `linux-amd64`, `linux-arm64`, `darwin-arm64`, `windows-amd64`. A commit that is only "green on the platform you happened to run" is NOT a release candidate: a test can pass on Linux allocators yet fail deterministically on macOS. Pin every step below to that commit with `--commit <sha>` so nothing drifts as `main` advances.

## Steps

1. **Sync tooling.** `git fetch origin main`, then `./make` so `bin/release` reflects latest main. Confirm `<sha>` is an ancestor of `origin/main` (`git merge-base --is-ancestor <sha> origin/main`).

2. **Drive all-platform CI.**
   - `bin/release ci all --commit <sha>` — a single `platform=all` run (one concurrency group, no self-cancellation), pinned to an immutable `ci-pin-<sha>` ref.
   - Poll to completion; all 4 platform jobs must be green. If any is red, STOP and diagnose — do not cut.
   - ci.yml uses `cancel-in-progress` per `github.ref`, so never dispatch overlapping runs on the same ref; the `--cancel-running` guard blocks an accidental overlap.

3. **Cut next.** `bin/release cut next --commit <sha>` — force-moves the `epoch-next` tag and pushes, triggering release.yml to build the pre-release.

4. **Wait for the epoch-next release build — THIS is the real gate, not CI.**
   - Find the run: `gh run list --workflow release.yml` (ref `epoch-next`, headSha `<sha>`). Poll to completion.
   - EVERY compiler job (including linux-arm64 **thin AND full**) and the `publish` job must succeed. This exercises `bin/release build -tags=embed_stub/embed_llvm` and the LLVM embed — a path ci.yml (which builds via `bin/build`, no embed tags) does NOT cover, so it can fail even when CI is fully green (e.g. a newly promoted platform missing its `stub_embed_<os>_<arch>.go` / `llvm_<os>_<arch>.go`).
   - If red: STOP, diagnose, file a tracker bug (never work around — CLAUDE.local.md), get the fix landed, and restart from step 2 at the new commit.

5. **Author synthesized release notes — NEVER the raw commit dump.**
   - `bin/release changes --commit <sha>` lists commits since the last stable epoch.
   - Synthesize into a themed markdown body: headline features, platform support, standard library, notable fixes (memory-safety / correctness), tooling & build. Reference tracker IDs. Keep it readable, not a 60-line subject dump.
   - Save it to a file (scratchpad). The install header is auto-prepended, so the file is just the body.

6. **Dry-run stable.** `bin/release cut stable --commit <sha> --notes-file <file> --dry-run` — confirm ALL gates are green (especially "epoch-next validated this SHA") and the notes render correctly. A gate is overridable only with `--reason "<text>"`; do NOT bypass CI / release-build gates for a stable release.

7. **Cut stable (irreversible, outward-facing).** `bin/release cut stable --commit <sha> --notes-file <file>`. Tags `epoch-<epoch>` @ `<sha>`, pushes the tag (triggers the stable release build), bumps `catalog.toml`, commits, and pushes `main`.

8. **Verify publish.** Poll the stable release.yml run (ref `epoch-<epoch>`) to green (all 4 compilers + publish). Then confirm `gh release view epoch-<epoch>` is published (not draft/prerelease) with thin+full assets for all 4 platforms + install scripts + `SHA256SUMS`, and that `latest` resolves to the new epoch (`gh release view --json tagName`).

9. **Write the narrative write-up for the user (required every stable cut — see [[feedback_release_writeup]]).** Cover what shipped (epoch, release commit/tag, headline features & platforms) and how it got there — blockers found and fixed, and any decisions (e.g. the commit choice, a platform that couldn't ship the originally-requested SHA).

## Rules
- Never work around a compiler / build / CI / test-infra blocker — file a tracker bug and get it fixed properly.
- The `cut` commands push by design (that IS the cut). A separate FIX commit still needs the user's explicit go-ahead before committing/pushing.
- Close or annotate tracker items shipped in the release.
- The annotated `epoch-*` tag object SHA differs from the commit it points at — resolve with `git rev-parse epoch-<epoch>^{commit}`.
