# Regression Prevention Gate System

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

## Class 1: Edit Gates

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

## Class 2: Commit Gates (Ratchets)

**Purpose:** Enforce that quality metrics only improve over time. Block commits that regress.

**Mechanism:** `bin/verify` writes a flat `gate-values.json` sidecar to `.promise-home/`. The commit gate (`tools/build/common/commitgate.go`) reads gate values directly by name and compares against baselines. No translation layer -- metric names in gate values match metric names in baselines.

### Gate Values

After a successful `bin/verify`, a sidecar file `.promise-home/gate-values.json` is written:

```json
{
  "timestamp": "2026-04-11T12:00:00Z",
  "platform": "darwin-arm64",
  "values": {
    "host_test_count": 3656,
    "host_leak_count": 0,
    "host_test_failures": 0,
    "wasm_test_count": 3397,
    "wasm_test_failures": 0
  }
}
```

Gate values are a flat `map[string]float64`. Adding a new metric requires only writing a new key in `verify.go` -- no mapping code needed.

### Baselines

Baselines live in `tools/gates/baselines.json` committed to the repo. This avoids the race condition where an agent updates a remote baseline before pushing the code -- other agents would trip the gate on stale code. Baselines travel with the code.

Each baseline entry has one of three states:

| State | Fields | Behavior |
|-------|--------|----------|
| **Enforced** | `direction` + `value` + `updated` | Ratchet-checked against gate values |
| **Pending** | `direction` only (no `value`) | Value auto-populated from next gate values run |
| **Informational** | `type: "informational"` | Tracked but not enforced; user adds `direction` to promote |

**Per-platform baselines** (WASM has more skipped tests, Windows excludes some tests, etc.):

```json
{
  "darwin-arm64": {
    "host_test_count": { "value": 3656, "direction": "up", "updated": "2026-04-11" },
    "host_leak_count": { "value": 0, "direction": "down", "updated": "2026-04-06" },
    "host_test_failures": { "value": 0, "direction": "exact", "updated": "2026-04-06" },
    "wasm_test_count": { "value": 3397, "direction": "up", "updated": "2026-04-11" },
    "wasm_test_failures": { "value": 0, "direction": "exact", "updated": "2026-04-06" },
    "coverage": { "direction": "up" },
    "binary_size": { "type": "informational" }
  }
}
```

The `coverage` entry above is **Pending** (has direction but no value -- will be auto-populated). The `binary_size` entry is **Informational** (tracked but not enforced).

**Ratchet directions:**
- `up`: value can only increase (more tests = better)
- `down`: value can only decrease (fewer leaks = better)
- `exact`: value must match exactly (zero failures)

**How it works:**
1. Reads `.promise-home/gate-values.json` (must be <10 min old)
2. Reads `tools/gates/baselines.json` for current platform
3. Auto-registers unknown gate values as **Informational** entries
4. Auto-populates **Pending** entries with the current value
5. Ratchet-checks **Enforced** entries against gate values
6. If regression: prints clear message, exits non-zero (commit blocked)
7. If improvement: updates `baselines.json` in-place, stages it with the commit
8. Queries tracker for active exceptions (gate ID + tracker bug ID + expiry)

**Defense-in-depth:** The `.githooks/pre-commit` hook runs a lightweight check that `baselines.json` values only improve vs. the committed version. Informational and Pending entries are skipped.

**Key files:**
- `tools/build/common/verify_summary.go` -- `GateValues` type + IO, `ParseTestSummaryLine`
- `tools/build/common/commitgate.go` -- `Baseline` struct (3-state), ratchet enforcement
- `tools/build/common/verify.go` -- writes `gate-values.json` after verify
- `tools/build/common/precommit.go` -- defense-in-depth baseline check
- `tools/gates/baselines.json` -- per-platform baseline state
- `.claude/skills/commit/SKILL.md` -- workflow integration

---

## Class 3: Periodic Gates

**Purpose:** Run slow quality checks on a schedule. Detect regressions that cannot be caught in the fast edit/commit loop.

**Mechanism:** The tracker schedules and dispatches periodic gates directly to agent runners via remote command execution. No AI involvement needed for most gates.

**Gate schedule:**

| Gate | Schedule | Filter | Command | Needs AI? |
|------|----------|--------|---------|-----------|
| `stress-mac` | Every 4h | os: darwin | `bin/stress.sh 1000` | No |
| `stress-linux` | Every 4h | os: linux | `bin/stress.sh 1000` | No |
| `stress-windows` | Every 4h | os: windows | `bin\stress.ps1 1000` | No |
| `stress-wasm` | Every 8h | any | `bin/promise test -target wasm32-wasi -stress 500 tests/...` | No |
| `size-wasm` | After every commit | any | `bin/size-report.sh --check` | No |
| `coverage-full` | Daily | any | `bin/coverage.sh` | Yes (analyze gaps) |
| `exception-audit` | Daily | any | `tools/gates/audit_exceptions.sh` | No |
| `agent-learning-cost` | Weekly | any | `tools/gates/learning_cost.sh` | Yes |
| `install-thin` | Daily | per-platform | `bin/gate install --variant thin --system` | No |
| `install-full` | Daily | per-platform | `bin/gate install --variant full --system` | No |

**Install gates (T0803).** `install-thin` / `install-full` validate the real end-to-end user install path — fetch the published install script, run it (download → checksum → decompress → `promise install`), sanity-check, then run the full test suite through the freshly **installed** binary (not `bin/promise`). Each runs in a clean ephemeral arena (hence `--system`: the arena *is* the clean environment). The **thin** variant exercises the first-compile blob-fetch path (network up); the **full** variant runs the suite online too (some tests legitimately fetch external catalog modules like `wasi_preview_2`), and *additionally* proves offline operation via a separate self-contained compile+run under a network blackhole (the `offline` phase) — confirming the host LLVM toolchain blobs were pre-staged at install. While the repo is private they point `PROMISE_BASE_URL` at the prebuilts dist bucket ([release-automation.md](release-automation.md) §5a; removed by T0804). Heavy (full install + full suite), so daily.

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

## Class 4: Platform Gates

**Purpose:** Verify that commits work on platforms not available on the committing host.

**Mechanism:** After a successful commit, the tracker dispatches verification to agents on other platforms via remote command execution.

**Flow:**
1. Agent commits on macOS, `/commit` skill notifies tracker: "commit `abc123` ready for platform verification"
2. Tracker creates platform gate runs for each required platform
3. Tracker dispatches to idle agents on target platforms:
   - Linux agent: `git pull && bin/verify`
   - Windows agent: `git pull && bin\verify.exe --wasm`
4. Agent runner executes command, returns stdout/stderr to tracker
5. Tracker parses result, updates gate status
6. If failure: tracker creates a bug, tags with `gate,platform,<platform>`

**Platform matrix:**

| Committing Host | Required Platform Gates |
|-----------------|------------------------|
| macOS | linux-amd64, windows-amd64 |
| Linux | darwin-arm64, windows-amd64 |
| Windows | linux-amd64, darwin-arm64 |

WASM is cross-platform (runs anywhere with wasmtime) so it is a commit gate, not a platform gate.

---

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

## Multi-Agent Safety

**Edit gates:** Each agent runs independently. Gates loaded from same `edit_gates.json` in repo.

**Commit gates:** Baselines in repo travel with the code. When agent A improves leak count and agent B improves test count on parallel branches, the merge combines both improvements. The pre-commit hook validates baselines only improve vs. the current branch state.

**Periodic gates:** Tracker assigns at most one agent per gate run. No double-execution.

**Platform gates:** Tracker ensures each (commit, platform) pair is verified exactly once.

---

## Implementation Phases

### Phase 1: Edit Gates
- Extend `guard.go` for Edit/Write hooks
- Create `edit_gates.json` with `no-allow-leaks` gate
- Update `.claude/settings.json`

### Phase 2: Commit Gates
- Create `commit_gate.go` (cross-platform)
- Create `baselines.json` with current platform metrics
- Update `/commit` skill and `.githooks/pre-commit`

### Phase 3: Periodic Gates
- Extend tracker with periodic gate scheduling + OS-aware dispatch
- Create `bin/size-report.sh`
- Create exception audit script
- Define initial periodic gate schedule

### Phase 4: Platform Gates
- Extend tracker with commit-triggered platform dispatch
- Update `/commit` to notify tracker
- Set up agent runners on Linux/Windows hosts

### Phase 5: Agent Learning Cost
- Design standard task set for agent evaluation
- Create measurement framework
- Add as weekly periodic gate
