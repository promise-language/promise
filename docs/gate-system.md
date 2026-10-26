# Regression Prevention Gate System

> **Tag:** `gate-system` — remaining work to complete this document: the query named in [docs/index.md](index.md)

## Overview

The Promise compiler uses a four-class gate system to prevent quality regressions across multiple dimensions (tests, memory leaks, stability, binary size, performance) and multiple platforms (Linux, macOS, Windows, WASM). The **tracker MCP server** is the central coordinator for scheduling, exceptions, and cross-platform dispatch.

## Gate Taxonomy

| Class | Name | Latency | Enforcement Point | Examples |
|-------|------|---------|-------------------|----------|
| 1 | **Edit gates** | <1s | PreToolUse hook on Edit/Write | Block `allow_leaks: true` in `.pr` files |
| 2 | **Commit gates** | 2-5 min | After verify, before commit | Leak count must not increase, test count must not decrease |
| 3 | **Periodic gates** | 10-60 min | Scheduled by tracker | Stress tests, coverage analysis, compilation time, binary size, agent learning cost |
| 4 | **Platform gates** | 5-30 min | After commit, dispatched by tracker | Verify on Windows, verify on Linux |

## Architecture

```
                          +----------------------------+
                          |     Tracker MCP            |
                          |     (central brain)        |
                          |                            |
                          |  - gate exception mgmt     |
                          |    (via web UI)            |
                          |  - periodic gate sched     |
                          |  - platform gate dispatch  |
                          |  - remote command exec     |
                          |  - agent status/idle       |
                          +-------------+--------------+
                                        |
              +-------------------------+-------------------------+
              |                         |                         |
     +--------+--------+      +--------+--------+      +---------+-------+
     |  macOS host      |      |  Linux host      |      |  Windows host   |
     |  agents 1-9      |      |  agents 1-N      |      |  agents 1-N    |
     |                  |      |                  |      |                 |
     |  Edit gates      |      |  Edit gates      |      |  Edit gates    |
     |  Commit gates    |      |  Commit gates    |      |  Commit gates  |
     |  Agent runner    |      |  Agent runner    |      |  Agent runner  |
     +------------------+      +------------------+      +-----------------+
```

Edit and commit gates run locally (fast, synchronous). Periodic and platform gates are dispatched by the tracker to agent runners directly -- no AI involvement needed for most of these.

---

## Class 1 Edit Gates

**Purpose:** Prevent forbidden patterns from being written to files. Instant, zero-latency enforcement.

**Mechanism:** The `tools/guard/guard.go` PreToolUse hook handles Edit and Write tool inputs in addition to Bash commands. Three PreToolUse matchers in `.claude/settings.json` (Bash, Edit, Write) all route to the same guard binary.

**Gate definitions** in `tools/gates/edit_gates.json`:

```json
{
  "gates": [
    {
      "id": "no-allow-leaks",
      "pattern": "allow_leaks\\s*:\\s*true",
      "files": "*.pr",
      "reason": "Adding allow_leaks:true is prohibited. Fix the leak or file a bug."
    },
    {
      "id": "no-skip-verify",
      "pattern": "--no-verify",
      "files": "*",
      "reason": "Skipping git hooks is prohibited."
    },
    {
      "id": "no-force-push-in-scripts",
      "pattern": "push\\s+--force",
      "files": "*.sh",
      "reason": "Force push in scripts is prohibited."
    }
  ]
}
```

**How it works:**
1. Guard binary detects tool type from input JSON:
   - Edit: has `file_path`, `old_string`, `new_string` -- checks `new_string`
   - Write: has `file_path`, `content` -- checks `content`
2. Loads gate definitions from `tools/gates/edit_gates.json`
3. For matching file globs, checks content against regex patterns
4. Returns deny with the gate's reason message
5. Queries tracker for active exceptions; if tracker unreachable, no exceptions apply (fail-closed)

**Key files:**
- `tools/gates/edit_gates.json` -- gate definitions
- `tools/guard/guard.go` -- PreToolUse hook (Bash + Edit + Write)
- `.claude/settings.json` -- hook registration

---

## Class 2 Commit Gates

**Purpose:** Enforce that quality metrics only improve over time. Block commits that regress.

**Mechanism:** `bin/verify` and every gate write a flat `gate-values.json` sidecar to `.promise-home/`, whose metric names match the names in `baselines.json` — no translation layer. Advancing a baseline from it is the **workspace's** job, uniform across every managed repository; this project carried its own `bin/commitgate` for it until that tool was deleted, unwired, in favour of the shared one. Nothing in this repository moves a baseline.

### Non ratchet commit checks

Not every commit-time check is a ratcheted metric. Pass/fail invariants that have no baseline to move are enforced elsewhere, and which tool carries which is [build-tools.md](build-tools.md#pre-commit-hook)'s to say: the hook (`bin/precommit-guard`) takes the checks that need the staged set — commit identity, staged-file rules, formatting, the ratchet below — and `bin/verify`'s structural phase takes the sweeps that do not, among them three **documentation checks** (`common.CheckDocs`, T1675):

- **Dangling Markdown links** -- every relative `.md` link target across all git-tracked Markdown must exist on disk. Scoped by `git ls-files`, so generated and untracked trees are skipped without an ignore list.
- **Index coverage** -- every tracked `docs/*.md` is linked from `docs/index.md`. Top level only; `docs/archive/` is intentionally unindexed.
- **Catalog coverage** -- every `modules/` directory has a `catalog.toml` entry, and every catalog module that ships real source is named in both `CLAUDE.md` and `docs/standard-library.md`.

**What these do not cover:** they verify link *targets*, index reachability, and module-name *presence* -- nothing about whether the surrounding prose is true. The failure mode that motivated them (T1675) was docs listing API names that do not exist and modules described as "planned" long after they shipped; that class of drift is not mechanically detectable and still needs a human sweep. The numeric facts that *are* derivable but still hand-maintained -- the per-module line and test counts in `docs/standard-library.md`, and the test-count snapshot in `README.md` -- are tracked separately as T1682. `README.md` is deliberately excluded from catalog coverage -- its module list is an ASCII repo tree of bare directory names, where a matcher robust enough to parse it would generate false positives.

### Gate Values

After a successful `bin/verify`, a sidecar file `.promise-home/gate-values.json` is written:

```json
{
  "timestamp": "2026-04-11T12:00:00Z",
  "platform": "darwin/arm64",
  "worktree": "9f2c...e1",
  "values": {
    "unformatted_files": 0,
    "go_unformatted_files": 0,
    "promise_unformatted_files": 0,
    "build_failures": 0,
    "check_findings": 0,
    "go_check_findings": 0,
    "failed_tests": 0,
    "go_failed_tests": 0,
    "promise_test_count": 11186,
    "promise_leaked_tests": 0
  }
}
```

Gate values are a flat `map[string]float64`. What verify writes is **exactly the
`integration` envelope's metrics** and nothing of its own: verify's test phase is
that gate, so a key here is a number some gate reported and can reproduce. It
used to add `wasm_*` keys from suites `integration` does not measure at all
(T2170). A new metric therefore arrives by a gate reporting it -- no mapping code
anywhere.

The `worktree` field is the **identity of the tree the values were produced from** (`common.WorktreeHash`): a SHA-256 over every file git considers part of the project -- tracked, plus untracked and not ignored -- each contributed as path, length and bytes. Two paths are excluded, each because including it would break the gate rather than for tidiness: `.promise-home/`, which holds this sidecar itself, and `tools/gates/baselines.json`, which a *passing* gate rewrites (and which is never an input to any gate value).

Two scoping decisions define what the identity covers, and both are deliberate:

- **Untracked-but-not-ignored files are in.** `bin/verify` compiles what is on disk, not what is in the index, so a file nobody has staged yet is part of what was tested. Hashing only tracked files would readmit the false pass: write a new file after verify, then commit it. Scratch that should not count belongs in `.gitignore` or outside the repo -- expressed once, for every tool, rather than as a second exclusion list here.
- **The index is out.** The identity is a function of worktree **content** alone, so `git add` and `git commit` leave it unchanged and no verify is spent on staging. The consequence is explicit rather than inherited: the gate vouches for the *worktree*, so a partial `git add` commits a subset of the tree that was verified -- the same scope the other worktree-reading pre-commit checks (formatting, docs) already assume.

Within that scope, any content edit that would reach a commit changes the identity.

This is what makes gate values *fresh*, and it replaces the wall-clock window the gate used to apply (T1962). The question the gate asks is "were these values produced from the tree I am about to commit?" -- a content question with an exact answer. A timestamp answered a different question ("was the sidecar written recently?") and was wrong in both directions: it rejected an unchanged tree once a long verify outlived the window, and it accepted values describing a *different* tree whenever an edit landed inside it. `InvalidateGateValues` (called by `bin/build` and `./make`) remains as a cheap "definitely invalid" fast path, but it is no longer load-bearing for correctness.

### Baselines

Baselines live in `tools/gates/baselines.json` committed to the repo. This avoids the race condition where an agent updates a remote baseline before pushing the code -- other agents would trip the gate on stale code. Baselines travel with the code.

A baseline is the best a metric has been. The file is keyed by metric, and each entry is an inclusive bound in one direction — `at_least` for a number that must not fall (a test count), `at_most` for one that must not rise:

```json
{
  "promise_leaked_tests": { "direction": "at_most", "value": 0 },
  "promise_test_count":   { "direction": "at_least",
                            "targets": { "darwin/arm64": 11186, "linux/amd64": 10708 } }
}
```

- **One `value` holds on every target.** A metric that genuinely differs by platform records its baselines per target, under `targets`, keyed `os/arch` as an envelope names its target; never both. A target with no figure is not judged by that baseline.
- **There is no informational or pending state.** A metric with no term is simply not judged, so tracking a number nobody judges needs no entry, and an entry is always a rule.
- **One metric has one direction** across `thresholds.json` and `baselines.json`.
- **The judge reads the file strictly** ([terms files](#the-contract-gates-and-the-judge)): an unknown key, a missing field, an unknown direction, both `value` and `targets`, or a metric no gate declares is refused, naming the file and the entry.

**A count of things being wrong is capped at zero, never baselined.** A metric that counts
something that should not happen — a failure, a leak, a diagnostic, an unformatted or
unbuildable file — is not a quality that improves over time: the only correct value is `0`, so
each is written as an `at_most 0` **cap** in `thresholds.json`, on its own name or on the shared
total it is part of. Which metrics those are is not a second list to keep in step: every metric
`integration` reports is one of them unless `integrationUnjudged` or `integrationCosts` says why
not, and `TestCorrectnessMetricsAreCappedAtZero` holds the caps and that set equal.
A ratchet from a non-zero value would be a standing allowance — it says "this many are
fine" and leaves a number a later reader can edit — so a measurement worse than the term is a
**defect to fix, never a number to write down**: resolving an item may not move a baseline to
gain forgiveness. Such a metric may still carry a baseline, and it still ratchets; it records
progress and never grants permission.

**Nothing in this repository moves a baseline.** Advancing one, from gate values whose `worktree` identity matches the tree being committed, is the workspace's job, uniform across every managed repository.

**Defense-in-depth:** The `.githooks/pre-commit` hook refuses a staged `baselines.json` that moves any enforced metric the wrong way against `HEAD`, on every target rather than only the host's, and refuses a removal as well as a raise — so neither editing an entry nor deleting it gets a commit through. The hook also rejects commits when running the formatter would introduce changes, so unformatted code never reaches origin and surfaces as a spurious diff the next time someone runs verify. Both are the commit gate's, not this repository's — see [build-tools.md](build-tools.md#pre-commit-hook).

**Key files:**
- `tools/build/common/verify_summary.go` -- `GateValues` type + IO, and the `Verify Summary` block
- `tools/build/common/hash.go` -- `WorktreeHash`, the content identity gate values are stamped with
- `tools/build/common/terms.go` -- reads `thresholds.json` and `baselines.json`, strictly, into the terms `bin/run` judges against. Reading only: nothing here moves a baseline.
- `tools/build/common/verify.go` -- measures `integration` in-process, judges it, and writes that envelope's metrics to `gate-values.json`
- `tools/build/common/blessing.go` -- the blessing: the tree identity, the record, and the one rule under which a passing `integration` verdict is recorded
- `tools/gates/thresholds.json`, `tools/gates/baselines.json` -- the caps and the baselines
- `.claude/skills/commit/SKILL.md` -- workflow integration

---

## Class 3 Periodic Gates

**Purpose:** Run slow quality checks on a schedule. Detect regressions that cannot be caught in the fast edit/commit loop.

**Mechanism:** The tracker schedules and dispatches periodic gates directly to agent runners via remote command execution. No AI involvement needed for most gates.

**Gate schedule:**

| Gate | Schedule | Filter | Command | Needs AI? |
|------|----------|--------|---------|-----------|
| `stress-mac` | Daily | os: darwin | `bin/run tested:stress` | No |
| `stress-linux` | Daily | os: linux | `bin/run tested:stress` | No |
| `stress-windows` | Daily | os: windows | `bin/run tested:stress` | No |
| `stress-wasm` | Every 8h | any | `bin/promise test -target wasm32-wasi -stress 500 tests/...` | No |
| `size-wasm` | After every commit | any | `bin/run size:wasm` | No |
| `coverage-full` | Daily | any | `bin/run covered` | Yes (analyze gaps) |
| `exception-audit` | Daily | any | `tools/gates/audit_exceptions.sh` | No |
| `agent-learning-cost` | Weekly | any | `tools/gates/learning_cost.sh` | Yes |
| `install-thin` | Daily | per-platform | `bin/run install:thin` | No |
| `install-full` | Daily | per-platform | `bin/run install:full` | No |

**Install gates (T0803).** `install:thin` / `install:full` validate the real end-to-end user install path — fetch the published install script, run it (download → checksum → decompress → `promise install`), sanity-check, then run the full test suite through the freshly **installed** binary (not `bin/promise`). Each installs into a sandbox of its own, never into the machine's `~/.promise`. The **thin** variant exercises the first-compile blob-fetch path (network up); the **full** variant runs the suite online too (some tests legitimately fetch external catalog modules like `wasi_preview_2`), and *additionally* proves offline operation via a separate self-contained compile+run under a network blackhole (the `offline` phase) — confirming the host LLVM toolchain blobs were pre-staged at install. They install straight from GitHub releases ([release-automation.md](release-automation.md), under [End to end install gate](release-automation.md#end-to-end-install-gate)). Heavy (full install + full suite), so daily.

**Filter model:** Each gate can filter by:
- `os`: darwin, linux, windows (detected from agent environment)
- `name`: specific agent name pattern
- `any`: runs on whoever is idle

The tracker knows each agent's OS and idle status, dispatching to the right target.

**Agent learning cost gate:** Measures how quickly/accurately an AI agent can learn the Promise language and complete a set of standard tasks. Tracks whether language changes make the language harder for agents to use -- a core design goal of Promise.

**Result handling:**
- Gate passes: tracker records success + timestamp
- Gate fails: tracker auto-creates a bug (type: `bug`, tags: `gate,<gate-name>`)
- Flaky tests: tracker cross-references existing flaky bugs

---

## Class 4 Platform Gates

**Purpose:** Verify that commits work on platforms not available on the committing host.

**Mechanism:** After a successful commit, the tracker dispatches verification to agents on other platforms via remote command execution.

**Flow:**
1. Agent commits on macOS, `/commit` skill notifies tracker: "commit `abc123` ready for platform verification"
2. Tracker creates platform gate runs for each required platform
3. Tracker dispatches to idle agents on target platforms:
   - Linux agent: `git pull && bin/verify`
   - Windows agent: `git pull && bin\verify.exe`
4. Agent runner executes command, returns stdout/stderr to tracker
5. Tracker parses result, updates gate status
6. If failure: tracker creates a bug, tags with `gate,platform,<platform>`

**Platform matrix:**

| Committing Host | Required Platform Gates |
|-----------------|------------------------|
| macOS | linux-amd64, windows-amd64 |
| Linux | darwin-arm64, windows-amd64 |
| Windows | linux-amd64, darwin-arm64 |

WASM is cross-platform (the pinned `wasmtime` is staged for every supported host) so it is a commit gate, not a platform gate.

---

## Test records

The suite gates — `tested:go`, `tested:promise`, `tested:wasm`, `tested:wasm-web`, and the suite `install:*` runs through an installed compiler — read one record per test and turn the records into an [envelope](#the-contract-gates-and-the-judge): a **group** per test file (per Go package), a **member** per test that did not pass, and the counts. This section is the contract for those records and that mapping. It is not printed by any command, because a command printing an embedded copy is a second copy that drifts from this one silently.

### Single target invariant

One gate run reports exactly one target, stamped once at the top of the envelope — never per record — as Go's own `os/arch` pair of the host it ran on (`linux/amd64`). The WASM suites are gates of their own, `tested:wasm` (wasm32-wasi, under the pinned `wasmtime`) and `tested:wasm-web` (wasm32-web, under the pinned Node 20+ and the embedded harness `compiler/cmd/promise/wasm_web_harness.js`), each reporting the host it ran on and judged against that host's terms. Both runtimes are pinned prebuilts staged on demand, not host installs ([runtime-architecture.md](runtime-architecture.md#wasm-runtime-sources)) — so what a gate can report does not turn on what a machine happens to have. Keeping them apart still matters: a host that cannot obtain one runtime reports the other, rather than the pair failing together.

### Suite envelope

```json
{
  "schema_version": 1,
  "gate": "tested:promise",
  "target": "linux/amd64",
  "metrics": [
    { "name": "failed_tests", "type": "int", "value": 1 },
    { "name": "promise_failed_tests", "type": "int", "value": 1 },
    { "name": "leaked_tests", "type": "int", "value": 0 },
    { "name": "promise_leaked_tests", "type": "int", "value": 0 },
    { "name": "test_count", "type": "int", "value": 6133 },
    { "name": "promise_test_count", "type": "int", "value": 6133 }
  ],
  "groups": [
    {
      "name": "tests/std/bool_test.pr",
      "members": [
        { "name": "test_or", "outcome": "fail", "detail": "panic: assertion failed: ...", "elapsed": 0.002 }
      ]
    }
  ]
}
```

### Test identity

Identity is the pair **(`file`, `test`)** and is **stable across runs** — it never varies with outcome, so a test that flips pass↔fail keeps the same identity.

- `file` is the test source path **relative to the repository root**, with forward slashes. No repository URL or absolute prefix. The runner emits an absolute path; the gate relativizes it against the root it knows authoritatively.
- `test` is the test-function name for batch files (one record per `` `test `` function), or the literal `"main"` for e2e / snapshot (`main()` `` `test ``) files — so every record has a non-empty test name and batch/e2e identities are uniform.

Tests excluded from compilation by a `` `target(...) `` **declaration** annotation are never compiled and produce **no record**. A `test(exclude: <target>)` test is compiled but skipped for the matching target and is reported with status `excluded` (see below).

For **`tested:go`**, `file` is the Go package as a repo-relative directory (the module prefix `github.com/promise-language/promise/` is stripped, so `.../compiler/internal/codegen` → `compiler/internal/codegen`) and `test` is the Go test-function name (subtests use `TestFoo/sub`). Go `SKIP` maps to status `excluded`.

### Status vocabulary

| status     | meaning |
|------------|---------|
| `pass`     | the test ran and passed |
| `fail`     | the test ran and failed (assertion/panic); `context` carries (bounded) detail |
| `timeout`  | the test exceeded its per-test timeout, or was the first test to not report before the batch budget expired (T1639) |
| `leak`     | the test leaked memory (alloc count delta > 0) |
| `memory`   | the test tripped the per-test memory limit (process aborted) |
| `excluded` | a `test(exclude: <this target>)` test, compiled but not run for this target |
| `not-run`  | the test never ran because an earlier test aborted the process |

**Abort attribution.** A `memory` abort, a process death mid-batch, and a batch-budget kill all terminate the whole test process without the runner naming the offending test by line. The runner attributes these from the file's roster (the ordered list of test functions, known at compile time):

- **MEMLIMIT abort** → the first roster test with no result is marked `memory`; every later roster test is `not-run`.
- **Batch-budget kill** (the test process outlived Σ per-test timeouts + 30s) → the runner prints a synthetic `TIMEOUT (-)` line naming the first unreported roster test, which is marked `timeout`; every later roster test is `not-run` (T1639). If every roster test *did* report and only the process teardown wedged, the line reads `TIMEOUT (-) <teardown>` and no test is blamed — the run still exits non-zero on the human path, but the gate sees each test's own real result.
- **Process exits with _any_ status (including 0) without reporting a result for a roster test** → the first unseen roster test is marked `fail` (with a crash / "process exited before reporting a result" `context`); every later roster test is `not-run`. The runner also prints a synthetic `INCOMPLETE` line naming that first unreported test, lists every unreported test in its context line, and exits non-zero — so the human path (`bin/verify`, interactive runs) sees the truncation too, not just the gate (T1415).

Tests that completed before the abort keep their real result.

### Metrics

Metrics are **derived by counting records**, so they always agree with the members. Each suite reports its counts twice: under the name every suite shares and under its toolchain's own (`promise_`, `wasm_`, `wasm_web_`, `go_`), so a composition can total the shared name while each toolchain's figure stays readable by itself. The full set is always present, even when a count is zero: a gate reports a stable metric set rather than omitting a zero.

| metric | counts records with status |
|--------|----------------------------|
| `test_count`, `<toolchain>_test_count` | `pass` |
| `failed_tests`, `<toolchain>_failed_tests` | `fail`, `timeout`, `memory`, `not-run` |
| `leaked_tests`, `<toolchain>_leaked_tests` | `leak` (the Promise suites) |
| `failed_packages`, `go_failed_packages` | Go packages `go test` reported `FAIL` (the Go suites) |

An `excluded` record is counted by none of them, and is never a member: a test skipped for this target did not fail.

### Store metrics

**Every gate reports what its run cost the content-addressed store.** Two quantities move bytes that no test asks for and no test controls — blobs pulled over the wire because the store did not have them, and bytes written exploding store (and embedded) content into usable form: the `llvm-view` tools, the CRT/OpenSSL/compiler-rt/winlink trees, the WASM objects, the macOS SDK stub, the embedded catalog. Both are side effects of the **tree's shape**, and both multiply by however many isolated caches a change decides to create, so a change that triples them must be visible as a number rather than as a timeout somewhere unrelated months later.

| metric | what it counts | end state |
|---|---|---|
| `cas_network_bytes` | bytes transferred over the wire into the store | `at_most 0` — a run that reaches the network for a blob is a cache-population defect, not a slow day |
| `cas_home_count` | distinct Promise homes the run reached the toolchain from | `at_most 1` — one warm home per run |
| `cas_materialized_bytes` | bytes written making delivered content usable | reported by `promise test` only; differs by platform by construction |
| `cas_materializations` | distinct view/tree populations | reported by `promise test` only; moves with how warm the host was |

- **Counted as spent, not as declared.** A transfer contributes the bytes that actually moved — a compressed source its compressed size, a transfer that failed verification what it pulled before failing — and a store hit contributes nothing. A count taken from manifest sizes would read the same warm or cold, which is the one distinction these exist to make.
- **A link costs nothing, and says so.** Linux symlinks its toolchain view and Windows hardlinks it; only macOS owns its bytes, because it patches and re-signs them. `cas_materialized_bytes` therefore differs by platform by construction, which is why it is no gate's metric — and why `cas_home_count`, which does not, is the one that carries the enforced term.
- **The window is the measured phase.** A gate brings the build up to date, materializes the toolchain once, empties the ledger, and only then measures. A fresh clone must stage a toolchain at some point; charging the first run for it would make the numbers describe the machine. Anything fetched or exploded *after* that point is work the tree asked for a second time.
- **Zero is a legitimate, visible value.** The full set is reported on every run, zeros included: a counter that appeared only when non-zero could not be judged against a term of zero.
- **Where they cannot be measured, they are omitted and the reason is given** — never reported as zeros, which read exactly like a clean run.
- **A home outside the worktree is refused by name, whatever the count.** Every tree gate measures in the worktree's own home, pinned by the build it performs first ([build-tools.md](build-tools.md#test-sandboxing)), so a ledger naming any other home — `~/.promise`, or a directory the caller exported — means the run measured the machine's shared state rather than only the tree. The envelope still carries both numbers, and its `incomplete` names each such home. The count alone could not say this: a run whose every compiler used `~/.promise` counts one home and reads as clean.
- **`integration` carries the two whose end state is an absolute.** `cas_network_bytes` and `cas_home_count` describe what a run ought never to do, so they belong with the numbers a landing decision rests on. The byte and population totals move with how warm the host was and with which platform links rather than copies; they are reported by `promise test`, where no term is owed.
- **A number is judged where it is reproducible; it *ratchets* only where it is stable.** The gates measure with `-count=1`, so each reports the same figure every time and a term on it means something. `bin/verify` judges these two on the same terms — its verdict is the judge's verdict on `integration`'s envelope, and a metric the gate enforced while verify ignored it would be a change verify blesses and `bin/run integration` rejects — but it deliberately keeps them out of its gate values, because its Go phase replays whatever the test cache holds and a ratchet fed from a cached run settles below every full one.
- **The install gate reports none of them.** It deliberately runs an installed compiler under a sandbox home, so its home count is two by construction and describes the gate rather than the tree.

The single source for all four is a ledger the compiler appends to, one line per occurrence, at `.home/tmp/.promise-cas.jsonl` in the worktree holding its binary — and beside that binary when it is not in a worktree, as an installed compiler is not. It is anchored to the *binary* and not to a Promise home on purpose: a run fans out into many compiler processes, those processes may each choose a different home, and a per-home tally cannot see the very case that matters — which is exactly how three private homes went unnoticed for eighteen days. The binary's own directory is `bin/`, which only `./make` and `workspace setup/update` may write ([build-tools.md](build-tools.md#tool-inventory)), so in a worktree the scratch root stands in for it; either way the path is a pure function of the binary, so every process in the run agrees on one file without being told. Appends are atomic, so concurrent compilers need no lock, and nothing is written on the warm path.

### Runner stream

The Promise suite gates are built on `promise test --json`, which streams one JSON record per line (newline-delimited JSON) as each test completes — robust to abrupt termination, since only a trailing partial line can be lost. Each line carries an **absolute** `file`, plus `test`, `status`, `elapsed`, and optional `context`. The gate parses these, relativizes the paths, groups by file, and derives the metrics above; a record's `context` becomes its member's `detail`.

Under `-coverage` the same stream also carries one **coverage record** per file — `{"kind":"coverage","file":…,"covered":N,"total":M}` — counting executed and total instrumented blocks. A coverage record has a `kind` and no test identity, so a reader keying on (`file`, `test`) skips it and the two record kinds coexist on one stream. Only `covered` runs the runner this way; the test gates never pass `-coverage`.

The stream also carries exactly one **store record** — `{"kind":"cas","network_bytes":N,"materialized_bytes":M,"materializations":K,"available":true}` — reporting what that run cost the store ([above](#store-metrics)). Like the coverage record it carries a `kind` and no test identity, and unlike the human output it is emitted on every run with its zeros present. On the human path the same numbers are one line after the summary block, printed **only when something was actually spent**: a zero-valued counter must not cost every warm run a line of a tail-read.

A record's `context` is bounded (≈50 lines / 4 KB, with a `… (truncated)` marker) before it enters the envelope, and the envelope as a whole within 1 MiB — past that, members' details go first, then members past the fiftieth of a group; the metrics are never touched. A failure that dumps a large body (e.g. a Go test printing the full generated IR) would otherwise JSON-encode onto a single multi-MB line, which the runner's line-oriented drain cannot consume — deadlocking the gate to its wall-clock timeout (T0777). The full, untruncated output still reaches the gate's stderr/console log.

---

## The contract gates and the judge

These are the tools the organization's tooling reaches a project through — `bin/gate`, `bin/run`, `bin/verify` and `./make` — and each answers the same contract, so a runner, a release trial and a person at a terminal all meet one surface. The runners **fail closed**: a gate or judge that cannot answer means *no*.

| | Invocation | Reads | Stdout | Status |
|---|---|---|---|---|
| Gate | `bin/gate <name> --envelope` | nothing | one [envelope](#envelope) | 0 if written, whatever it measured; 1 if nothing could be measured |
| Judge | `bin/run <name> --verdict` | the envelope, on stdin | one verdict | 0 if written, whatever it says; 1 if none can be reached |
| By hand | `bin/run <name>` | nothing | each metric beside every term; in JSON `{"envelope","verdict"}` | 0 acceptable; 1 not, or nothing measured |
| Listing | `bin/gate --list` | nothing | `{"gates":[{"name","summary","metrics","requires","serialize"?}…]}` | 0 |
| Listing | `bin/run --list` | nothing | `{"commands":[{"name","serialize"?}…],"gates":[names…]}` | 0 |
| Command | `bin/run <command> [args…]` | nothing | whatever `bin/<command>` prints | its own; 3 `unbuilt` if `bin/<command>` is absent |
| Verify | `bin/verify` | nothing | the summary: the stage roll, or `{"ok","stages","tree"?}` | 0 every stage passed; 1 otherwise |

### Conventions every tool follows

- **Flags.** `-name` and `--name` are one flag; names are lowercase whole words (`-h` is not a flag); a value follows `=` or is the next argument. The command path comes first — the gate name, or the gate or command name — and flags after it in any order; `--` ends the flags.
- **Usage errors exit 2**, write nothing on stdout, and name every problem on stderr: an unknown flag (with the closest real one and a pointer to `-help`), an unknown command, a flag given twice, `-json` with `-human`. A bare `bin/gate` or `bin/run` is one too: it says what it expects, its `-version` line and its description.
- **Output mode.** JSON when stdout is not a terminal, human when it is; `-json` and `-human` force one. `--envelope` and `--verdict` fix their output by a contract of their own, so they refuse either. Stdout carries only the result; progress is narration, on stderr only when someone is watching — stderr is a terminal, or `-narrate` was given.
- **`-json-input <path>`** reads the parameters from one JSON object: a key per parameter, `true`/`false` for a boolean, `"output-mode"` for `-json`/`-human`, and `"args"` for the command path (`{"envelope": true, "args": ["tested:go"]}` is `tested:go --envelope`). An unknown key is an unknown flag, and a parameter given both there and on the command line is refused — there is no precedence.
- **`-help`** prints every flag with its spellings and type, and the command class with the command that lists its members — never the members. **`-version`** prints `{"project":"promise","text":"<tools source hash>"}`, the hash the staleness check compares.
- **Four exit statuses**, and no other: 0 done; 1 could not complete, or the subject failed; 2 malformed invocation; 3 refused.
- **A tool not fit to act refuses**, before it reads its command line, `-help` and `-version` included: `stale` (the tools source changed since `./make` built it), `unstamped` (not built by `./make`), `repository-unreachable` (its stamped root is gone), and `unbuilt` (`bin/run <command>` with no binary). It exits 3 with `{"condition","recovery":"./make"}` as its result; in the two protocol modes, `--envelope` and `--verdict`, stdout stays empty and the status and one stderr line carry it. A runner that meets one runs `./make` and asks once more. `./make` itself is never stale.

### The listing

**Every row carries what the gate reports.** `metrics` is every metric its envelope carries, with its `type` (`int` or `float`) and `unit` (`bytes`, `percent`, or none), so terms can be written against a gate nobody has run yet; `requires` is always present, `[]` while the gate runs no fetched program; `serialize: "host"` marks a gate that holds the machine's exclusion while it measures, which a runner takes before spawning it. `bin/run --list` names exactly the gates `bin/gate --list` does, and every command `./make` builds as an object row — `verify` declaring `serialize: "host"` — so the listing answers in a fresh clone; a name that is both is an error. **The command set is closed**: nothing answers outside the listing.

**The JSON listing is an array of objects, not of names.** A caller addresses a gate by `name`; a field added later is ignored by a reader rather than refused. The distinction is not cosmetic: flow's discovery unmarshals each entry into a struct with a `name`, so a bare `["tested", …]` still parses, yields nothing, and the repository is discovered as **a machine with no gates** — a silent failure with no parse error anywhere to explain it.

### Envelope

One JSON object on stdout, written whole and at most 1 MiB: `schema_version` 1, the `gate` asked for, the `target` it speaks for (`os/arch`), `metrics` (`name`, `type`, `value`, and `unit` where declared — an `int` stays an integer), `groups` naming where each number came from with the `members` that did not pass in the tool's own words ([test records](#test-records)), and `incomplete_reason` only when the run measured less than a full one. Completeness is that reason's absence. A complete envelope carries every metric its gate declares, each at most once.

**`tree` is this project's own field**: the git tree id the measurement speaks for, stamped only when the identity before and after the run agreed, and absent on a gate whose subject is the machine. The judging layer blesses from it ([build-tools.md](build-tools.md#the-blessing)); a reader that does not know it skips it.

### The gates

A name is a concept and an optional instance (`checked:go`), and every row is separately runnable — that is what narrowing means here. A concept gate reports its concept's metrics under the shared name and, beside it, under each toolchain's own (`go_check_findings`, `promise_check_findings`); a composition's shared name is the total over its parts.

| Gate | Measures | Metrics |
|---|---|---|
| `formatted:go` | Go files `gofmt` would rewrite — without rewriting them | `unformatted_files`, `go_unformatted_files` |
| `formatted:promise` | `.pr` files `promise format` would rewrite | `unformatted_files`, `promise_unformatted_files` |
| `formatted` | both of the above | all of the above |
| `builds` | Go packages that fail to compile, per module — and whether the build itself completed | `unbuildable_packages`, `go_unbuildable_packages`, `build_failures` |
| `checked:go` | `go vet` diagnostics, generated code excluded | `check_findings`, `go_check_findings` |
| `checked:promise` | `promise check` over every Promise unit this project owns | `check_findings`, `promise_check_findings`, `promise_check_failures`, `promise_check_warnings`, `promise_check_units` |
| `checked` | both of the above | all of the above |
| `tested:go` | failing tests in every Go module's suite | `failed_tests`, `failed_packages`, `test_count`, each also `go_` |
| `tested:promise` | failing tests in the host Promise suite | `failed_tests`, `leaked_tests`, `test_count`, each also `promise_` |
| `tested` | both host suites | all of the above |
| `integration` | `formatted` + `builds` + `checked` + `tested`, measured at once — what a landing decision rests on | all of the above |
| `fit` | the machine, before work is given to it | `worktree_free_bytes`, `cache_free_bytes`, and on Unix `worktree_hides_unlinked_files` |
| `tested:wasm` | the Promise suite against wasm32-wasi, under the pinned `wasmtime` | `failed_tests`, `leaked_tests`, `test_count`, each also `wasm_` |
| `tested:wasm-web` | the Promise suite against wasm32-web, under the pinned Node | the same, each also `wasm_web_` |
| `tested:stress` | tests that do not agree with themselves across repeated runs | `stress_flaky_count`, `stress_iterations` |
| `covered` | how much of each language's source the suites reach | `statement_coverage`, `go_statement_coverage`, `block_coverage`, `promise_block_coverage` (`percent`) |
| `size:wasm` | what the wasm32-wasi canaries compile to, per canary | `wasm_size_<canary>`, `wasm_size_total` (`bytes`) |
| `install:thin`, `install:full` | installing a published release of that variant, end to end | the install phases' own metrics |
| `latest-invariant` | whether `releases/latest` resolves to an epoch-* release | `latest_is_epoch` |

Every gate above except `fit`, `install:*` and `latest-invariant` also reports `cas_network_bytes` and `cas_home_count` — see [Store metrics](#store-metrics).

**The last eight are not part of `integration`, and that is the point of naming them.** They are the measurements a landing decision must not wait on — the wasm suite alone runs longer than every host gate combined — and they are run on a schedule instead. Naming them is what lets an orchestrator ask for one.

**Three of those names are outside the closed concept vocabulary** — `size:wasm`, `install:*` and `latest-invariant`. Two carry an instance half even though the concept is ours, because each measures one of several things it could: the canaries are built for wasm32-wasi, and a later `size:native` is a sibling gate rather than a rename of this one. A variant is always part of the name, never a gate's argument: a gate takes no argument but `--envelope`.

### Terms and the verdict

- **Two files, read strictly.** A **cap** is an absolute a person edits, in [`tools/gates/thresholds.json`](../tools/gates/thresholds.json): `{"<metric>": {"direction": "at_most" | "at_least", "cap": <number>}}`. A **baseline** is the best a metric has been, in [`tools/gates/baselines.json`](../tools/gates/baselines.json) ([Baselines](#baselines)). Both are inclusive bounds. The judge refuses, naming the file and the entry, an unknown key, a missing field, an unknown direction, a baseline with both `value` and `targets`, a metric no gate declares, and one metric with two directions.
- **Every cap and baseline a metric has is applied, and all must hold.** An integer metric is compared as an integer. A metric with no term is not judged. `thresholds` holds one object per judged metric with only the keys that applied — `{"direction", "cap"?, "baseline"?}` — so anyone holding the envelope can re-check the verdict.
- **A refusal says what to do.** `detail` states the judgement (metric, value and term), the evidence (up to five members that neither passed nor were excluded, each in the tool's own words, and a count of the rest — or that the gate reported nothing about why), and the remedy each gate states once in its own definition, naming the command that re-measures it.
- **An unjudgeable envelope gets no verdict at all.** One that is not JSON, carries no or another `schema_version`, names another gate, repeats a metric, reports a metric as another type or unit than its gate declares, claims to be complete while lacking a declared metric, or carries no metric any term speaks to: the judge exits 1 having written nothing. So does an invalid terms file. An unjudged measurement is not a pass.
- **An incomplete run is never a pass.** A run that measured less than a full one is judged and never acceptable, and `detail` relays its reason whole: honest numbers that understate what was checked are indistinguishable from an improvement.
- **The judge spawns nothing.** It reads stdin and the terms files, so anyone holding both reaches the same verdict. `bin/run <name>` without `--verdict` is the by-hand path: it spawns `bin/gate <name> --envelope` as a process — the same boundary a runner crosses — and judges what that printed. No decision rests on it.

- **`integration` is a composition, and every part is addressable.** A step fixing Go vet findings runs `bin/run checked:go` (seconds) rather than paying for the whole suite each round. Passing the parts is not passing the whole, and only the whole may be cited: a fix for one area can break another, and a sequence of narrow passes describes no single state.
- **A gate reports what its run cost the store, once.** The bytes a run pulls over the wire and the bytes it explodes out of the store are properties of the whole run, not of any part of it — a composition's parts all draw on the same store — so the numbers appear exactly once in the envelope. Reporting them per part would put the same bytes in it several times under one name and leave the judge with duplicates. The window belongs to the **process**, `bin/gate` itself, and not to the function that measures: measuring is something a caller may do at any time, and a window is a side effect no measurement should carry — one opened inside a function call resets the ledger of whatever run is already measuring. `fit` is the exception, as ever: it measures the machine, must answer on one that cannot build, and so neither warms a toolchain nor opens a window.
- **`fit` is not part of `integration`.** A machine that cannot build is not a change that may not land. It reports free space on the worktree's filesystem (which also holds `.promise-home/`) and on the one under `go env GOCACHE`, always both, so the envelope's shape does not vary by host. On a Unix host it also reports `worktree_hides_unlinked_files`, capped at 0: whether the worktree's filesystem keeps a removed file's name while the file is still open. A filesystem served through libfuse's high-level API, such as ntfs-3g, renames such a file to `.fuse_hidden…` until its last close, and NFS to `.nfs…`, so a temp dir a test has just emptied cannot be removed — every suite that runs a binary out of a temp dir, and the `go` tool's own work dir, then fails in cleanup after the test itself passed. The suites assume a removed name is gone at once everywhere but Windows, whose cleanup waits the lag out, so the metric is not reported there. A checkout on such a filesystem is a machine that cannot be given work, not a change that may not land; moving the checkout to a native filesystem clears it.
- **The gate never judges; the judge never measures.** A gate reads no term, so it cannot be made to pass by editing it; the terms are a separate artefact out of the subject's reach, read by `bin/run` alone ([Terms and the verdict](#terms-and-the-verdict)).
- **Every metric `integration` reports carries a term on every target this project ships**, or a written reason in `integrationUnjudged` not to — not merely on whichever host happens to be running. A term is per-target data where it is a baseline with `targets`, so a check on the host alone is blind to the platform its author is not sitting on, and the gap arrives as a red trunk for the next person there instead of a failure for whoever opened it; `TestIntegration_EveryMetricIsJudgedOnEveryShippedTarget` walks every shipped target.
- **Every gate measures the tree as it is now, never a leftover build.** Before it measures, a gate brings the build up to date by calling `bin/build`'s own `RunBuild` **in-process** — the same parser generation, resource embedding and compile — and adds no freshness rule of its own: `bin/build` decides what "up to date" means, so an already-built tree costs about 0.1s. It happens once per gate process, so `integration`'s parts share one build. This is not an optimisation of `tested`: the compiler `go:embed`s about a dozen generated, untracked inputs that only `bin/build` produces, so a gate that measured without producing them first reported numbers about a tree nobody proposed (stale artifacts), or refused to measure at all (missing artifacts), or reported a `go_check_findings` count made entirely of the resulting package-load errors — a number about a tree `vet` never got far enough to analyse. `fit` is the only exception: it measures the machine rather than the tree, and must be answerable on a machine that cannot build. A build that does not complete is itself a measurement — `builds` reports `build_failures`, and every part whose numbers would be about build artifacts reports none and says why.
- **A gate never repairs its subject.** Every check is the check-only form — no `gofmt -w`, no `promise format`, no staging; that is the difference from `bin/verify`, which repairs on its way to an answer. Bringing the build up to date is not a repair, but it is not invisible either: when the grammar has moved since `compiler/internal/parser/` was generated, the build regenerates those tracked files, and a runner enforcing the non-modification rule reports that as `broke_contract`. That reaches only a change that edited the grammar without regenerating, and the regenerated sources are left in the worktree for that change's own commit to carry.
- **A project tool and the gate that measures the same property are one implementation in two modes.** The tool repairs, the gate measures, and that is the *only* difference between them: neither re-spells the other's subject, module list, package selection, exclusions or analyzer set. `bin/check` and `checked:go` share `GoCheckFindings` — the gate's count is the size of the list the tool prints. `bin/format` and `formatted:go` share one walk; `bin/coverage` and `coverage` share one package selection; `bin/test` and `tested:go` share one `go test` argv. Two spellings drift, and a drifted pair gives the project two contradictory answers to one question with no way to tell which is right.
- **`checked:promise` measures UNITS, not files.** A unit is what can be analysed on its own: a directory with a `promise.toml` — all of its `.pr` files, tests included — or a `.pr` file that belongs to no project. A file of a multi-file module is not one, and never was: `modules/std/vector.pr` checked alone reports `_FnIter` undefined because `_FnIter` is in `iter.pr`. A per-file count therefore measured how the files are arranged rather than whether the code is sound, which is why the gate did not exist until `promise check` could take a project. `promise_check_failures` counts units that did not check and `promise_check_findings` the diagnostics in them, because a unit already carrying findings would otherwise absorb another one silently; `promise_check_warnings` counts diagnostics the compiler builds through; `promise_check_units` is the scale of what was swept, and is not judged for the reason a suite's `test_count` is not.
- **The Promise checker's tool mode is `bin/promise check` itself.** The compiler IS the checker, so there is no `bin/` wrapper: a person fixing type errors runs the same command the gate runs — `bin/promise check tests/... modules/... examples/... tools/stub/...` — and the gate's only addition is counting the summary line that command already prints. Its subject is `promiseSuiteTargets()`, the same list `tested:promise` runs, so the two measure the same code and a target added for one is added for both.
- **Every Go gate sweeps every Go module in the tree** — `compiler/`, `tools/build/`, and `flows/` when this clone has one. `./...` is module-scoped, so measuring one module would report honest numbers about part of the subject. A module that is present but cannot be resolved (`flows/` without `flow-sdk/` beside it) is named in `incomplete`, never silently skipped.
- **Generated code is excluded from being checked and from coverage, and never from being built.** `compiler/internal/parser` is written by ANTLR from the grammar, so a finding there is not actionable by the author of a change — it has ~77 `unreachable code` sites by construction. It still has to compile, so `builds` includes it. The exclusion is applied to the diagnostics and not only to the package list, because `go vet` reports findings in a package's *dependencies*: leaving the parser out of the list left its findings in the count, and whether they surfaced at all varied with the host's Go release and build-cache state — a metric that moves with that describes the host rather than the tree.
- **A gate reports the tree on the pinned toolchain.** With any `PROMISE_*` toolchain override in effect (`PROMISE_OPT`, `PROMISE_LLC`, `PROMISE_LLD`, `PROMISE_LD64LLD`, `PROMISE_WASM_LD`, `PROMISE_CLANG`, `PROMISE_USE_CLANG`), the measurement would describe the operator's machine instead, and cannot stand as evidence about a `(commit, platform, check)` triple — so `bin/gate` refuses before any measurement starts, printing the reason on stderr and no envelope at all (T2108). A by-hand run under an override is still available through `bin/test` / `bin/verify`, which announce the override and proceed.
- **Stdout carries one JSON object and nothing else.** `bin/gate <name>` without `--envelope` measures nothing: it is a usage error (exit 2) naming `bin/run <name>` as the form for a person, so a bare run can never be mistaken for a pass. An error path prints nothing at all: a reader gets one envelope or none.
- **`thresholds` is always present in a verdict**, even when empty: a verdict without the terms it was reached from cannot be re-checked by anyone who was not there.

- **A measurement says which tree it is about, and a passing `integration` verdict blesses it.** A gate stamps its envelope with the tree identity it saw, taken after the build and again when the measurement is done; if the two differ it carries none, so a tree edited mid-measurement blesses nothing. Recording is the **judging** layer's, never the gate's — a gate cannot know whether its numbers are acceptable — so `bin/run integration`, `bin/run integration --verdict` and `bin/verify` all reach one rule (`blessIfPassed`), and one passing measurement of a tree is sufficient for the commit guard whoever ran it. Only the whole may be cited here too: a green part blesses nothing. See [build-tools.md](build-tools.md#the-blessing).

**`bin/verify` measures this gate.** Its test phase is `integration`, run through the same in-process entry point `bin/run integration` uses, and its verdict is the judge's verdict on that envelope — so "verify passed" and "integration passed" are one answer about one tree rather than two that can differ. Verify's additions are the ones a gate may not make: it repairs first (`gofmt -w`, `promise format`), and it records the blessing.

## Exception Management

All gate exceptions are managed through the **tracker web UI** -- no file editing required.

An exception record:
- **Gate ID**: which gate is excepted (e.g., `no-allow-leaks`)
- **Tracker ID**: the bug/task that justifies the exception (e.g., `B0300`)
- **Scope**: file pattern or platform (e.g., `tests/concurrency/known_leak.pr`)
- **Expiry**: auto-expires after N days (default 7)
- **Granted by**: user name (agents cannot self-grant)
- **Reason**: human-readable justification

Edit gate and commit gate binaries query the tracker for active exceptions. If the tracker is unreachable, exceptions do not apply (fail-closed).

---

## Model Invocation

A gate run must never be able to invoke a model. Gates are unattended CI: they run on every host, hourly, forever, with nobody reading the launch — so a gate that can reach a model burns account quota on a schedule with no human in the loop and no cost ceiling. This is a property of gate execution, enforced rather than incidental:

- **Statically**: `bin/gate`'s sources (`tools/build/common`, `tools/build/cmd/gate`) are scanned for any agent entry point — `claude`, `bin/do`, `bin/flow`, an Anthropic SDK import, or an MCP client. A future edit that adds one fails this test rather than shipping quietly.
- **At runtime**: `bin/gate` sets `PROMISE_GATE=1` at the start of every run, inherited by its entire subprocess tree. `bin/tool-guard` (the PreToolUse hook the workspace installs) denies every prompt-invoking tool — `Task`/`Agent`, `Skill`, any `mcp__*` tool, and bash invocations of `claude`, `bin/do`, or `bin/flow` — whenever that marker is present. Consistent with that guard's general fail-closed contract: if the guard cannot tell whether a tool dispatches a model, it denies.

---

## Multi Agent Safety

**Edit gates:** Each agent runs independently. Gates loaded from same `edit_gates.json` in repo.

**Commit gates:** Baselines in repo travel with the code. When agent A improves leak count and agent B improves test count on parallel branches, the merge combines both improvements. The pre-commit hook validates baselines only improve vs. the current branch state.

**Periodic gates:** Tracker assigns at most one agent per gate run. No double-execution.

**Platform gates:** Tracker ensures each (commit, platform) pair is verified exactly once.

---

## Implementation Phases

### Phase 1 Edit Gates
- Extend `guard.go` for Edit/Write hooks
- Create `edit_gates.json` with `no-allow-leaks` gate
- Update `.claude/settings.json`

### Phase 2 Commit Gates
- Create `commit_gate.go` (cross-platform)
- Create `baselines.json` with current platform metrics
- Update `/commit` skill and `.githooks/pre-commit`

### Phase 3 Periodic Gates
- Extend tracker with periodic gate scheduling + OS-aware dispatch
- Create `bin/size-report.sh`
- Create exception audit script
- Define initial periodic gate schedule

### Phase 4 Platform Gates
- Extend tracker with commit-triggered platform dispatch
- Update `/commit` to notify tracker
- Set up agent runners on Linux/Windows hosts

### Phase 5 Agent Learning Cost
- Design standard task set for agent evaluation
- Create measurement framework
- Add as weekly periodic gate
