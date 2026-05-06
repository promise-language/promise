---
name: coverage
description: Analyze test coverage for a Go package or Promise module, identify gaps, write missing tests, and file bugs for issues found.
---

Analyze test coverage and fill gaps. If $ARGUMENTS is provided, treat it as a Go package path (e.g., `./internal/codegen/`), a Promise test directory (e.g., `tests/e2e/`), or a specific source file to focus on.

## Tracker status

Before starting, update your tracker status: call `mcp__tracker__heartbeat` with `status: "reviewing"` (keep the same agent, item_id, item_title from earlier phases). If you haven't registered yet, determine your agent name via `basename $(pwd)` and hostname via `hostname`.

## Steps

1. **Identify scope.** If arguments were given, use them. Otherwise, check `git diff HEAD~1 --name-only` to focus on recently changed code.

2. **Measure current Go coverage** (if reviewing Go code).
   - Run `go test ./<package>/ -coverprofile=/tmp/cov.out -count=1` from `compiler/`.
   - Run `go tool cover -func=/tmp/cov.out` and note functions with low coverage (below 70%).
   - Read the source of uncovered functions to understand what they do.

3. **Measure current Promise coverage** (if reviewing Promise code).
   - Run `bin/promise test -coverage <file-or-dir>` to get block-level coverage.
     - Single file: `bin/promise test -coverage tests/e2e/foo_test.pr`
     - Directory: `bin/promise test -coverage tests/e2e/...`
     - Multiple targets: `bin/promise test -coverage tests/std/... modules/...`
   - Or use `bin/coverage.sh promise <target>` for a formatted report.
   - Note: coverage instruments function/method entries, if/else branches, while/for/loop bodies, and enum match arms. Only user code is instrumented — std/modules, test functions, and `main()` are skipped.
   - Note: files containing only test functions (no non-test code) will show no coverage data. This is expected — coverage measures how well tests exercise non-test code.

4. **Audit Promise test coverage** (if reviewing Promise code).
   - Read the source `.pr` file(s) and identify all public types, methods, functions, and edge cases.
   - Find corresponding `*_test.pr` files (co-located) or tests in `tests/`.
   - List untested or under-tested functionality: missing edge cases, error paths, boundary conditions.
   - Cross-reference with `bin/promise test -coverage` output to identify uncovered functions/branches.

5. **Audit resource invariants** (not just code path coverage).
   - For every type in the scoped code: if it heap-allocates (native, contains `pal_alloc`, or has droppable fields), verify there's a test that confirms cleanup happens. If no test exists and no drop path exists, file a **critical** bug for the leak.
   - For concurrency code: verify there's a stress test. If missing, file a task.
   - Coverage percentages can be misleading — 100% line coverage doesn't mean resources are properly managed. Think about *what invariants* are tested, not just *which lines* are hit.

6. **Write missing tests.**
   - **Go tests**: Follow existing patterns — `generateIR()` + `assertContains` for codegen, `checkErrs()` + `expectError` for sema, `ownerOK()` / `ownerErrs()` for ownership.
   - **Promise tests**: Use batch tests (`` `test `` annotation with `assert()`) unless testing exact output. Co-locate `*_test.pr` files with source files for modules; use `tests/` for cross-cutting e2e tests.
   - Prioritize: error paths, edge cases, and recently changed code over happy-path coverage that already exists.

7. **Verify.** Run the new tests to confirm they pass. For Go: `go test ./<package>/ -run <TestName> -v -count=1`. For Promise: `bin/promise test <file>`.

8. **File bugs.** If you discover untestable code (missing language features, compiler bugs, test infra limitations), file with `mcp__tracker__create` rather than working around the issue.

9. **Report.** Summarize: coverage before/after (for Go and Promise), what tests were added, and any bugs filed.
