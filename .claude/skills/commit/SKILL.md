---
name: commit
description: Verify, commit, and sync changes. Updates docs and tracker, files bugs for any issues noticed, runs full verification before and after rebase.
---

Verify, commit, and sync the current changes with remote.

## Tracker status

Before starting, update your tracker status: call `mcp__tracker__heartbeat` with `status: "committing"` (keep the same agent, item_id, item_title from earlier phases). If you haven't registered yet, determine your agent name via `basename $(pwd)` and hostname via `hostname`.

## Steps

1. **Update docs.**
   - If the changes affect documented behavior, update relevant docs (`docs/`, CLAUDE.md, module `doc` annotations).

2. **Build.**
   - Run `./build` (Linux/macOS) or `.\build.ps1` (Windows) to ensure the binary is up-to-date with any source or module changes. Never run `go build` directly.

3. **Pre-commit verification.**
   - Run `bin/verify.sh --local --wasm` (Linux/macOS) or `powershell -ExecutionPolicy Bypass -File bin\verify.ps1 -Local` (Windows) from the repo root.
   - If it fails, fix the issues and re-run until it passes. Do not proceed until green.
   - **Memory leak tracking**: After verify completes, check the output for leak counts (lines like `N leaked` in test summaries). Record the total leak count. Compare against the baseline (~2170 leaks). If your changes increased the leak count, investigate and fix the regression before proceeding. If your changes reduced the leak count, note the improvement in the commit message.

4. **Commit.**
   - Stage the relevant files (avoid `git add -A`; be specific).
   - Write a concise commit message: focus on the "why", not the "what". Follow the style of recent commits (`git log --oneline -10`).
   - If changes span multiple logical units, make separate commits.

5. **Sync and rebase.**
   - `git fetch origin` and `git rebase origin/master`.
   - If there are conflicts, resolve them carefully and continue the rebase.

6. **Post-rebase verification.**
   - Run `bin/verify.sh --local --wasm` (Linux/macOS) or `powershell -ExecutionPolicy Bypass -File bin\verify.ps1 -Local` (Windows) again to confirm nothing broke during rebase.
   - If it fails, fix and re-run.
   - **Memory leak check**: Verify the leak count hasn't increased compared to pre-rebase. If rebase introduced new leaks (from upstream changes), note them but don't block the commit — file a bug instead.

7. **Update tracker.**
   - Update any related tracker entries (`mcp__tracker__update`) — mark bugs/tasks as done.
   - **Set the `summary` field** when closing an item (status → `done`/`wontfix`/`works_as_intended`/`cant_reproduce`). The summary is markdown and should contain:
     - What was changed (one-line description of the fix/implementation)
     - Key details (files changed, tests added/removed, stale tags cleaned up)
     - Verify results (host + WASM test counts, 0 failures)
   - Example summary: `"Removed isErrorType exclusion from bindingFree — error instances now get pal_free at scope exit\n\nAdded clearDropFlag call in genRaiseStmt before emitScopeCleanup to prevent UAF\nAdded 2 Go IR tests\nRemoved 6 stale allow_leaks tags\n\nVerify: 3211 host + 2954 WASM tests passing, 0 failures"`
   - **File any issues discovered during this session.** Specifically check for:
     - Memory leaks: types without drop, allocations without free paths
     - Concurrency bugs: races, deadlocks, missing synchronization
     - Performance issues: unnecessary allocations, algorithmic complexity problems
     - Missing tests for critical invariants (cleanup, thread safety, error recovery)
   - Use `mcp__tracker__create` with priority **critical** for memory/concurrency issues, **high** for performance, **medium** for missing tests.

8. **Mark done.**
   - Call `mcp__tracker__heartbeat` with `status: "done"`, `item_id: ""`, `item_title: ""` to signal completion.
