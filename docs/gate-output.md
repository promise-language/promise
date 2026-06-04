# Gate output schema (T0763)

Every `bin/gate` subcommand emits a single JSON envelope (`GateOutput`) on
stdout. Human readable progress goes to stderr, so stdout is always clean
machine-readable JSON. Run `bin/gate schema` to print this contract.

## One envelope for all subcommands

| subcommand  | `target`       | `files` (per-test) | metrics |
|-------------|----------------|--------------------|---------|
| `test`      | host           | yes (Promise tests) | `host_*` |
| `wasm-test` | `wasm32-wasi`  | yes (Promise tests) | `wasm_*` |
| `go-test`   | host           | yes (Go tests, grouped by package) | `go_test_*` |
| `stress`    | host           | no                 | `stress_*` |
| `coverage`  | host           | no                 | `*_coverage_pct` |
| `wasm-size` | host           | no                 | `wasm_size_*` |

Metric-only gates (`stress`, `coverage`, `wasm-size`) omit `files`. Test gates
populate it. The envelope shape is identical so the tracker ingests one schema.

## Single-target invariant

One gate invocation reports exactly one target, stamped once at the top of the
envelope — never per record. `bin/gate test` reports the host (e.g.
`linux-amd64`) and is host-only; `bin/gate wasm-test` reports `wasm32-wasi`;
`go-test`/`stress`/`coverage`/`wasm-size` report the host they ran on. Any
unknown argument (including `-wasm` to `bin/gate test`) is rejected — wasm tests
are a separate single-target gate (`bin/gate wasm-test`).

## Envelope

```json
{
  "target": "linux-amd64",
  "metrics": {
    "host_test_count": 6133,
    "host_test_failures": 0,
    "host_leak_count": 0,
    "host_timeout_count": 0,
    "host_memory_count": 0,
    "host_excluded_count": 0,
    "host_not_run_count": 0
  },
  "files": [
    {
      "file": "tests/std/bool_test.pr",
      "tests": [
        { "test": "test_and", "status": "pass", "elapsed": 0.001 },
        { "test": "test_or",  "status": "fail", "elapsed": 0.002,
          "context": "panic: assertion failed: ..." }
      ]
    },
    {
      "file": "tests/e2e/hello.pr",
      "tests": [ { "test": "main", "status": "pass", "elapsed": 0.02 } ]
    }
  ],
  "complete": "promise-tests"
}
```

## Test identity

Identity is the pair **(`file`, `test`)** and is **stable across runs** — it never
varies with outcome, so a test that flips pass↔fail keeps the same identity.

- `file` is the test source path **relative to the repository root**, with
  forward slashes. No repository URL or absolute prefix. The runner emits an
  absolute path; the gate relativizes it against the root it knows
  authoritatively.
- `test` is the test-function name for batch files (one record per `` `test ``
  function), or the literal `"main"` for e2e / snapshot (`main()` `` `test ``)
  files — so every record has a non-empty test name and batch/e2e identities are
  uniform.

Tests excluded from compilation by a `` `target(...) `` **declaration** annotation
are never compiled and produce **no record**. A `test(exclude: <target>)` test
is compiled but skipped for the matching target and is reported with status
`excluded` (see below).

For **`go-test`**, `file` is the Go package as a repo-relative directory (the
module prefix `github.com/promise-language/promise/` is stripped, so
`.../compiler/internal/codegen` → `compiler/internal/codegen`) and `test` is the
Go test-function name (subtests use `TestFoo/sub`). Go `SKIP` maps to status
`excluded`.

## Status vocabulary

| status     | meaning |
|------------|---------|
| `pass`     | the test ran and passed |
| `fail`     | the test ran and failed (assertion/panic); `context` carries (bounded) detail |
| `timeout`  | the test exceeded its per-test timeout |
| `leak`     | the test leaked memory (alloc count delta > 0) |
| `memory`   | the test tripped the per-test memory limit (process aborted) |
| `excluded` | a `test(exclude: <this target>)` test, compiled but not run for this target |
| `not-run`  | the test never ran because an earlier test aborted the process |

### Abort attribution

A `memory` abort and a hard crash terminate the whole test process without the
runner naming the offending test by line. The runner attributes these from the
file's roster (the ordered list of test functions, known at compile time):

- **MEMLIMIT abort** → the first roster test with no result is marked `memory`;
  every later roster test is `not-run`.
- **Hard crash with no summary** → the first unseen roster test is marked `fail`
  (with a crash `context`); every later roster test is `not-run`.

Tests that completed before the abort keep their real result.

## Metrics

Metrics are **derived by counting records**, so they always agree with the
`files` data. Each is prefixed by the target family (`host_` or `wasm_`); the
full set is always present, even when a count is zero (a gate reports a stable
metric set rather than omitting a zero). The commit gate consumes these reported
values directly — it does not re-count records.

| metric                | counts records with status |
|-----------------------|----------------------------|
| `<p>_test_count`      | `pass` |
| `<p>_test_failures`   | `fail` |
| `<p>_leak_count`      | `leak` |
| `<p>_timeout_count`   | `timeout` |
| `<p>_memory_count`    | `memory` |
| `<p>_excluded_count`  | `excluded` |
| `<p>_not_run_count`   | `not-run` |

## Runner stream (`promise test --json`)

The gate is built on `promise test --json`, which streams one JSON record per
line (newline-delimited JSON) as each test completes — robust to abrupt
termination, since only a trailing partial line can be lost. Each line carries an
**absolute** `file`, plus `test`, `status`, `elapsed`, and optional `context`.
The gate parses these, relativizes the paths, groups by file, and derives the
metrics above.

A record's `context` is bounded (≈50 lines / 4 KB, with a `… (truncated)`
marker) before it enters the envelope. A failure that dumps a large body (e.g. a
Go test printing the full generated IR) would otherwise JSON-encode onto a
single multi-MB line, which the runner's line-oriented drain cannot consume —
deadlocking the gate to its wall-clock timeout (T0777). The full, untruncated
output still reaches the gate's stderr/console log.
