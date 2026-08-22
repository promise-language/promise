---
name: cut-release
description: Cut a stable Promise epoch release end-to-end — drive pinned per-platform CI cheapest-first, cut next, wait for the epoch-next release build, author synthesized "what changed" notes, cut stable, verify publish, and write a narrative summary for the user.
---

Cut a stable `epoch-<year>.<n>` release from a chosen commit. `bin/release` enforces the gates; this skill is the orchestration playbook around it, including the human-facing parts the tooling does NOT do (synthesized notes + a narrative write-up).

**Pick the release commit first.** Default to the current `origin/main` tip. It must be reachable from `origin/main` and must pass CI on ALL required platforms — `linux-amd64`, `linux-arm64`, `darwin-arm64`, `windows-amd64`. A commit that is only "green on the platform you happened to run" is NOT a release candidate: a test can pass on Linux allocators yet fail deterministically on macOS. Pin every step below to that commit with `--commit <sha>` so nothing drifts as `main` advances.

**Actions minutes are metered and this repo is private — spend them in cheapest-first order and stop at the first red.** macOS bills 10x and Windows 2x, so a parallel all-platform run turns one Linux-detectable regression into four paid runs. Step 2 spells out the order; do not shortcut it.

## Steps

1. **Sync tooling.** `git fetch origin main`, then `./make` so `bin/release` reflects latest main. Confirm `<sha>` is an ancestor of `origin/main` (`git merge-base --is-ancestor <sha> origin/main`).

2. **Drive CI ONE PLATFORM AT A TIME, cheapest first — never `platform=all`.**

   All four platforms must be green before you cut, but they must be proven **sequentially**, stopping at the first red. `platform=all` spins up all four runners at once, so a failure that a 1x Linux runner would have caught in minutes has already burned a full 10x macOS run and a 2x Windows run in parallel. Money already spent cannot be recovered by cancelling — the fix-and-re-run cycle then pays for all four *again*.

   Dispatch in this order, each pinned to the release SHA, waiting for each to conclude before dispatching the next:

   | Order | `bin/release ci <target> --commit <sha> --watch` | Runner | Billing |
   |---|---|---|---|
   | 1 | `linux-amd64` | `ubuntu-24.04` | 1x |
   | 2 | `linux-arm64` | `ubuntu-24.04-arm` | 1x (metered on private repos) |
   | 3 | `windows-amd64` | `windows-latest` | 2x |
   | 4 | `darwin-arm64` | `macos-latest` | 10x |

   - **Stop at the first red.** Do not dispatch the next platform. Diagnose, file a tracker bug, get the fix landed, then restart step 2 from `linux-amd64` at the new commit — a fix invalidates the green results you already have.
   - **Sequential dispatch is what makes this safe.** ci.yml's `concurrency: cancel-in-progress` is keyed on `github.ref`, and every pinned dispatch shares the same `ci-pin-<sha>` ref — so two *overlapping* dispatches would cancel each other. Waiting for each run to conclude before dispatching the next avoids that entirely. Use `--watch` (it polls to completion and exits non-zero on red) and never background two dispatches at once. `--cancel-running` exists as a guard against accidental overlap; do not use it to force a second concurrent run.
   - **If a run is already doomed, cancel it.** A red job in a multi-job run leaves its siblings burning for nothing (and `gh run view --log-failed` refuses to serve logs until the run concludes, so cancelling also unblocks diagnosis). `gh run cancel <run-id>`.
   - Cheapest-first is also fastest-feedback-first: most regressions are platform-independent and die on `linux-amd64`. The expensive platforms exist to catch what Linux cannot — allocator, ABI, path, and line-ending differences — so they are worth running only once Linux is green.

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

9. **Announce the release in GitHub Discussions → Announcements (public, maintainer voice).** After publish is verified (step 8), post an announcement to the **Announcements** category. Draft the body from the synthesized notes (step 5) in the maintainer's voice — plain and terse, no hype or emoji, honest that Promise is pre-1.0: a one-line "epoch-`<epoch>` is the current stable epoch", 3–5 headline bullets (the language/platform highlights, not the full changelog), links to the release tag + install doc, and a closing pointer to Q&A / Ideas. It should PARALLEL the GitHub release, not restate it — link to it. Save the body to a scratchpad file.
   - Get the ids once: `gh api graphql -f query='{ repository(owner:"promise-language", name:"promise"){ id discussionCategories(first:25){ nodes{ id name } } } }'` → the repo id and the **Announcements** category id.
   - Post it: `gh api graphql -f query='mutation($r:ID!,$c:ID!,$t:String!,$b:String!){ createDiscussion(input:{repositoryId:$r,categoryId:$c,title:$t,body:$b}){ discussion{ url } } }' -f r=<repoId> -f c=<announcementsCatId> -f t="Promise epoch-<epoch>" -F b=@<body-file>`. Announcements is maintainer-post-only, so this needs a token with Discussions write (`repo` scope). Report the discussion URL.

10. **Write the narrative write-up for the user (required every stable cut — see [[feedback_release_writeup]]).** Cover what shipped (epoch, release commit/tag, headline features & platforms) and how it got there — blockers found and fixed, and any decisions (e.g. the commit choice, a platform that couldn't ship the originally-requested SHA).

## Rules
- Never work around a compiler / build / CI / test-infra blocker — file a tracker bug and get it fixed properly.
- The `cut` commands push by design (that IS the cut). A separate FIX commit still needs the user's explicit go-ahead before committing/pushing.
- Close or annotate tracker items shipped in the release.
- The annotated `epoch-*` tag object SHA differs from the commit it points at — resolve with `git rev-parse epoch-<epoch>^{commit}`.
