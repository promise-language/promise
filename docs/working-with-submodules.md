# Working with the `flow` / `flow-sdk` submodules

The promise repository is a **superproject** with two git submodules:

| Submodule | Path | Origin | What it is |
|-----------|------|--------|------------|
| `flow` | `flow/` | github.com/promise-language/flow | OSS flow substrate (cli, StepCtx, Backend interface, git helpers) |
| `flow-sdk` | `flow-sdk/` | `ssh://hfe/git/tracker_flow_sdk.git` | Tracker backend (the `Backend`/`Agent` implementation the flows run on) |

They are wired into the `flows/` Go module via local `replace` directives, so the
flows build offline once the submodules are checked out. The compiler and the
project gates do **not** depend on either submodule — only `flows/` does.

This document exists because submodules confuse both humans and agents. Read the
**mental model** once; then each task maps to one of the three workflows below.

---

## Mental model: the gitlink is the contract

The superproject does **not** store the submodules' files. It stores, in its own
tree, a **gitlink** for each: a single commit SHA (`160000 commit <sha> flow`).
That SHA is the *only* source of truth for "which submodule commit this
promise revision was built and tested against."

Everything follows from three invariants:

- **I1 — Determinism.** `./make`, the gates, and `git submodule update` all
  materialize *exactly the recorded gitlink* — never "latest upstream". A given
  promise commit always builds against the same submodule commits.
- **I2 — No dangling gitlinks.** A promise commit's gitlink must point at a
  submodule commit that is **pushed** (reachable on the submodule's origin).
  Otherwise nobody else — including a fresh CI clone — can fetch it. ⇒ **push the
  submodule before the superproject.**
- **I3 — Explicit bumps.** Changing which submodule commit promise uses requires
  **staging the gitlink** (`git add flow`). Nothing advances it implicitly.

> **The single most important rule** (it prevents 90% of the pain): the moment you
> advance a submodule, **`git add` the gitlink**. `git submodule update` reads the
> SHA from the *index*. A staged gitlink ⇒ `./make` is a no-op. An *un*staged
> advance ⇒ `./make` silently reverts your submodule back to the old SHA.

### Why `./make` "reverts" your submodule

`./make` runs `git submodule update --init` (see
[`ensureFlowSubmodules`](../tools/build/cmd/make/main.go)). That command checks
out the **index** gitlink into the submodule worktree:

| Submodule state | `git submodule update` does |
|-----------------|------------------------------|
| Clean, at the gitlink SHA | nothing (no-op) |
| Clean, **committed ahead but gitlink not staged** | **reverts** worktree to the old gitlink SHA ← the surprise |
| **Staged** gitlink bump (`git add`) | checks out your new SHA (no-op) |
| Dirty (uncommitted edits) | fails → `./make` warns and keeps your tree |

So: **stage early** and `./make` never fights you.

---

## Setup (automatic)

`./make` applies the submodule-safety git config for you, idempotently, via
`RunSetup` (see [`tools/build/common/setup.go`](../tools/build/common/setup.go))
— there is **no manual one-time step**. After any `./make` your clone has:

| Key | Value | Why |
|-----|-------|-----|
| `push.recurseSubmodules` | `check` | **I2 safety net** — refuse to push the superproject if it references an unpushed submodule commit (no dangling gitlinks). |
| `status.submoduleSummary` | `true` | Show pending gitlink bumps in `git status`. |
| `diff.submodule` | `log` | Show submodule commit ranges in `git diff`, not a bare SHA. |
| `fetch.recurseSubmodules` | `on-demand` | Fetch submodule commits referenced by fetched superproject commits, so a gitlink bump is always resolvable. |

`submodule.recurse=true` is already set globally in this environment, so
`git pull` / `git checkout` keep submodules in step with the superproject's
gitlinks automatically.

To *develop* a submodule against its branch (so `git submodule update --remote`
and branch attach work cleanly), the tracking branch is `main` for both. Opt in
locally without touching `.gitmodules`:

```bash
git config submodule.flow.branch main
git config submodule.flow-sdk.branch main
```

---

## Workflow A — change a submodule and bump promise to it

Use this when a task requires editing `flow/` or `flow-sdk/` and promise must
adopt the change.

```bash
# 1. Attach the submodule to a branch (submodules check out detached by default).
git -C flow checkout main

# 2. Edit + commit INSIDE the submodule (it is a normal git repo).
#    (cd flow && edit … && git commit …)

# 3. Push the submodule FIRST (Invariant I2).
git -C flow push                       # or: git push --recurse-submodules=on-demand in step 6

# 4. Stage the gitlink bump in the superproject (Invariant I3 — do this NOW).
git add flow

# 5. Commit the superproject — gitlink bump together with the dependent promise
#    changes, so they land atomically.
git commit -m "…; bump flow to <short-sha>"

# 6. Push the superproject. With push.recurseSubmodules=check this is rejected
#    unless step 3 already pushed the submodule.
git push
```

`git status` (with `status.submoduleSummary`) shows the pending bump as
`modified: flow (new commits)` before step 4, and a staged gitlink after.

---

## Workflow B — safely rebase when the submodule head advanced

This is the painful case. There are two shapes.

### B1 — only *upstream* advanced; you have no submodule changes

Your promise commits don't touch the gitlink, so there is no gitlink conflict.

```bash
git fetch origin
git rebase origin/main
git submodule update --init --recursive   # materialize the (possibly newer) gitlinks
./make                                     # rebuilds flows against them
```

### B2 — you bumped the submodule **and** upstream bumped the same submodule

Rebasing replays *your* gitlink change onto a base whose gitlink also moved ⇒ a
**gitlink conflict**. Resolve **submodule-first, then superproject**:

```bash
# 1. Reconcile the submodule itself: put YOUR submodule work on top of upstream.
git -C flow fetch origin
git -C flow checkout main
git -C flow rebase origin/main         # your flow commits replayed onto upstream head
git -C flow push                       # publish before the super references it (I2)
TARGET=$(git -C flow rev-parse HEAD)

# 2. Rebase the superproject. On the gitlink conflict:
git rebase origin/main
#   → CONFLICT (submodule): flow
git -C flow checkout "$TARGET"         # ensure the worktree is the SHA you want
git add flow                           # resolving a gitlink conflict = stage current submodule HEAD
git rebase --continue

# 3. Verify determinism, then rebuild.
git submodule status                   # ' <TARGET> flow'  — no leading + or -
./make                                  # no-op, no revert
```

> **Golden rule for gitlink conflicts:** there are no conflict markers to edit — a
> gitlink is a tree entry. You resolve it by **checking out the submodule SHA you
> want and `git add <submodule>`**. Git records whatever the submodule's HEAD is
> at that moment. Get the submodule right *first*, then stage.

---

## Workflow C — pristine restore (gates, CI, automation)

Automated runs must reach a deterministic state: superproject at an exact
revision, submodules at *exactly* their recorded gitlinks (I1), no stray files.
This is **destructive** and must be run by trusted tooling, never by an agent
(the [guard](../flow-sdk/tools/guard/guard.go) blocks `git reset --hard` etc. for
agent-issued commands precisely so automation owns this step).

```bash
git fetch origin
git checkout --force <rev>                     # or: git reset --hard origin/main
git submodule sync --recursive                 # pick up any URL changes
git submodule update --init --recursive --force   # force submodules to recorded gitlinks
git clean -ffdx                                # remove untracked files in the super
git submodule foreach --recursive 'git clean -ffdx'
```

`--force` is what distinguishes this from `./make`'s gentle update: it discards a
committed-ahead submodule worktree to honor the recorded gitlink. That is correct
for a gate (it must test the recorded combination), and wrong for development
(it would throw away your in-progress submodule work) — which is exactly why the
two are separate modes.

After a pristine restore, `./make` rebuilds tools + flows against the recorded
gitlinks.

---

## do-flow & submodules

The do-flow's push step (`flows/do/steps.go`) commits locally and pushes under
the tracker push lease. When a task's work includes submodule changes:

1. The change must be **committed inside the submodule** and the gitlink staged in
   the super *before* the flow's commit step — the flow commits the superproject
   worktree, which records the gitlink, but it does **not** commit inside the
   submodule for you. (A task that needs submodule edits should make and commit
   them in the submodule as part of its implementation.)
2. The arena/runner that performs the push must have
   `push.recurseSubmodules=on-demand` (or `check` + an explicit submodule push) so
   the submodule commit is published before the superproject gitlink that
   references it (I2). Without it the pushed promise commit points at a submodule
   commit no other host can fetch.

If you hit a case where the flow cannot cleanly push a submodule-spanning change,
**file a tracker bug** rather than hand-hacking the gitlink — the cross-repo push
ordering is a real design surface, not something to paper over.

---

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| `./make` keeps reverting `flow` to an old commit | Submodule advanced but gitlink not staged (I3) | `git -C flow checkout <sha> && git add flow` |
| `git status`: `modified: flow (new commits)` | Submodule HEAD ≠ recorded gitlink | Intended? `git add flow`. Not intended? `git submodule update -- flow` |
| Submodule on "(HEAD detached at …)" | Normal after `submodule update` | To develop: `git -C flow checkout main` |
| Rebase: `CONFLICT (submodule): flow` | Both sides moved the gitlink (B2) | Checkout the desired submodule SHA, `git add flow`, `git rebase --continue` |
| Fresh clone / CI: `fatal: reference is not a tree: <sha>` | Gitlink points at an unpushed submodule commit (I2 violated) | Push the submodule commit; never reference unpushed submodule SHAs |
| `git push` rejected: "submodule … not pushed" | `push.recurseSubmodules=check` caught an unpushed submodule | Push the submodule first (Workflow A step 3) |

---

## Quick reference

```
git submodule status                 # show recorded gitlink + checkout state (+/- = drift)
git diff --submodule                 # show submodule commit ranges in a diff
git -C flow checkout main            # attach a submodule for development
git add flow                         # stage a gitlink bump (do this EARLY)
git submodule update --init -- flow  # restore ONE submodule to its recorded gitlink
./make                               # build; honors the *staged* gitlink
```

Three rules, in order of importance:
1. **Stage the gitlink the moment you advance a submodule** (`git add flow`).
2. **Push the submodule before the superproject** (no dangling gitlinks).
3. **Gates restore to the *recorded* gitlink; development advances it explicitly.**
