---
name: commit
description: Verify, rebase, and commit changes. Updates docs, files GitHub issues for any problems noticed, runs full verification before and after rebase. Does NOT push.
---

Verify, rebase, and commit the current changes. **Does NOT push** — the orchestrator handles pushing after independent verification.

## Steps

1. **Update docs.**
   - If the changes affect documented behavior, update relevant docs (`docs/`, CLAUDE.md, module `doc` annotations).

2. **Build.**
   - Run `bin/build` to ensure the binary is up-to-date with any source or module changes. Never run `go build` directly.

3. **Pre-commit verification.**
   - Run `bin/verify --wasm` from the repo root.
   - If it fails, fix the issues and re-run until it passes. Do not proceed until green.
   - **Memory leak check (ZERO TOLERANCE)**: After verify completes, check the output for leak counts (lines like `N leaked` in test summaries). **The repo has 0 leaks. Any leak in the output is a regression caused by your changes.** Do NOT treat any leak as preexisting — there are none. Fix all leaks before proceeding. Changes that introduce memory leaks will not be pushed.
   - **No `allow_leaks: true` tags — ever.** Check `git diff` for any added `allow_leaks: true` — this is a blocking issue. The repo has 0 `allow_leaks` tags. Never add them. If a test leaks, fix the leak. There are no exceptions.

4. **Commit gate.**
   - Run `bin/commitgate` from the repo root.
   - If it fails, a quality metric regressed — fix the regression before committing.
   - If it updates `tools/gates/baselines.json`, stage the updated file with the commit.

5. **Commit.**
   - Stage the relevant files (avoid `git add -A`; be specific).
   - Write a concise commit message: focus on the "why", not the "what". Follow the style of recent commits (`git log --oneline -10`).
   - If changes span multiple logical units, make separate commits.

6. **Sync and rebase.**
   - `git fetch origin` and `git rebase origin/main`.
   - If there are conflicts, resolve them carefully and continue the rebase.

7. **Post-rebase verification.**
   - Run `bin/verify --wasm` again to confirm nothing broke during rebase.
   - If it fails, fix and re-run.
   - **Memory leak check**: Verify the leak count is still 0 after rebase. Any leaks are a regression — fix before pushing.

8. **File any issues discovered during this session** on [GitHub Issues](https://github.com/promise-language/promise/issues). Specifically check for:
   - Memory leaks: types without drop, allocations without free paths
   - Concurrency bugs: races, deadlocks, missing synchronization
   - Performance issues: unnecessary allocations, algorithmic complexity problems
   - Missing tests for critical invariants (cleanup, thread safety, error recovery)

   Open issues with priority labels — **critical** for memory/concurrency issues, **high** for performance, **medium** for missing tests.
