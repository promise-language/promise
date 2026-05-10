---
name: stress
description: Run bin/stress 1000 (or the number given as arguments) and compare the flaky tests agains the currently logged flaky bugs. Consider the host target. When you see flaky bugs that no longer reproduce, close them. If you see new unfiled bugs file them
---

Run stress tests to detect flaky tests, then reconcile results against the bug tracker.

## Inputs

- `$ARGUMENTS` — optional iteration count or duration passed to `bin/stress` (e.g., `1000`, `500`, `30s`). Defaults to `1000` if omitted.

## Steps

### 1. Run stress tests

- Execute `TERM=xterm bin/stress $ARGUMENTS` (or `TERM=xterm bin/stress 1000` if no arguments given).
- This builds the compiler, then runs `bin/promise test -timeout 15s -stress <N> tests/... modules/...`.
- Let it run to completion. The output ends with a structured report.

### 2. Parse the report

Read the stress test output and extract:

- **Target**: the host platform line (e.g., `Target: linux-x86_64`). This is critical — flakiness is often platform-specific.
- **FLAKY tests**: test name, file, pass rate (e.g., `47/50 (94.0%)`), timing stats, failure type (fail/timeout), crash context (signal, stderr).
- **HIGH VARIANCE tests**: test name, file, CoV, timing stats.
- **STABLE count**: total stable tests and files.

### 2b. Check for resource leaks

- If the stress test ran long enough (>100 iterations or >60s), check for signs of resource leaks:
  - Monotonically increasing memory usage across iterations (if measurable from output)
  - Increasing test times in later iterations (may indicate accumulating leaked state)
  - OOM kills or system resource exhaustion
- If leak indicators are present, file a **critical** bug with tag `memory-leak` describing the observed behavior and which tests were running.

### 3. Fetch existing flaky bugs from tracker

- Call `mcp__tracker__list` with `type: "bug"`, `tag: "flaky"` to get all known flaky bugs.
- For each, call `mcp__tracker__get` to read full details (description, notes, status).
- Build a mapping of test file/name → tracker bug ID for cross-referencing.

### 4. Cross-reference results against known bugs

For each test in the stress report, determine which category it falls into:

| Stress result | Tracker state | Action |
|---------------|---------------|--------|
| FLAKY | No existing bug | **File new bug** (step 5) |
| FLAKY | Existing open bug | **Update bug** with new data point (step 6) |
| Not in FLAKY | Existing open bug for this test | **Close bug** — no longer reproduces (step 7) |
| HIGH VARIANCE only | No existing bug | Note in report but don't file (variance without failures isn't a bug) |
| HIGH VARIANCE only | Existing open bug | Keep open — may still be flaky at higher iteration counts |

### 5. File new flaky bugs

For each newly discovered flaky test, call `mcp__tracker__create` with:

- **type**: `bug`
- **title**: `Flaky: <test_name> in <file>` (e.g., `Flaky: test_channel_send in stress_unbuffered.pr`)
- **priority**: `medium` (default), escalate to `high` if pass rate < 90% or crash signal present
- **tags**: `flaky`, plus subsystem tags inferred from the file path:
  - `tests/concurrency/` → `concurrency`, `scheduler`
  - `tests/e2e/` → the relevant feature tag
  - `modules/` → the module name
- **description**: Include all available data:
  - Host target (e.g., `linux-x86_64`)
  - Pass rate (e.g., `47/50 (94.0%)`)
  - Timing stats (avg, σ, min, max)
  - Failure breakdown (N fails, N timeouts)
  - Crash context if present (signal name, stderr excerpt)
  - Iteration count and total duration of the stress run

### 6. Update existing flaky bugs

For bugs that are still flaky, call `mcp__tracker__update` to add a note with:

- Date and host target
- Current pass rate vs. previously recorded rate (improving/degrading/stable)
- Any new crash context or changed failure mode
- Update priority if severity has changed (e.g., pass rate dropped significantly)

### 7. Close resolved flaky bugs

For bugs whose tests ran stable across all iterations, call `mcp__tracker__update` with:

- **status**: `done`
- **note**: `No longer reproduces after N iterations on <target>. Closing.`

Be conservative: only close if the test ran a meaningful number of times (check the adaptive scheduling — stable tests may have been skipped after ~20 runs). If the iteration count is low (< 50 effective runs for that test), note this but still close — it can be reopened if it recurs.

### 8. Report

Give a concise summary:

- **Target**: host platform
- **Iterations**: count and duration
- **New bugs filed**: list with tracker IDs and pass rates
- **Bugs updated**: list with tracker IDs and trend (improving/degrading)
- **Bugs closed**: list with tracker IDs (no longer reproduces)
- **Still flaky**: list of known bugs that remain open
- **Stable**: total count

## Notes

- **Platform matters.** A test may be flaky on Linux but stable on macOS (or vice versa). Always include the host target in bug descriptions and notes. If a bug's description mentions a different platform than the current run, note the cross-platform data but don't close it — it may only reproduce on the other platform.
- **Adaptive scheduling.** The stress runner gradually reduces how often stable tests are executed (every 2nd/4th/8th iteration after 20/50/100 runs). A test marked STABLE may have only run ~30 effective times in a 1000-iteration run. This is still meaningful but less conclusive than 1000 full runs.
- **Don't file bugs for HIGH VARIANCE alone.** High timing variance without actual failures is a performance observation, not a bug. Only mention it in the report summary.
- **Crash context is valuable.** When a test crashes with SIGSEGV/SIGABRT, the stderr excerpt often contains the stack trace. Include it verbatim in the bug description — it's the most useful diagnostic data.
