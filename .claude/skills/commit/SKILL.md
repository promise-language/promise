---
name: commit
description: Verify, commit, sync, and push changes. Updates docs and tracker, files bugs for any issues noticed, runs full verification before and after rebase.
---

Verify, commit, sync with remote, and push the current changes.

## Session name

Before starting, update the session name to reflect the current phase. Replace the phase prefix (e.g., `COV`) with `COMMIT`, keeping the rest of the name: `/rename COMMIT <ID> <short-title>`. If the session has no name yet, skip this.

## Steps

1. **Update docs.**
   - If the changes affect documented behavior, update relevant docs (`docs/`, CLAUDE.md, module `doc` annotations).

2. **Build.**
   - Run `./build` (Linux/macOS) or `.\build.ps1` (Windows) to ensure the binary is up-to-date with any source or module changes. Never run `go build` directly.

3. **Pre-commit verification.**
   - Run `bin/verify.sh --local --wasm` (Linux/macOS) or `powershell -ExecutionPolicy Bypass -File bin\verify.ps1 -Local` (Windows) from the repo root.
   - If it fails, fix the issues and re-run until it passes. Do not proceed until green.

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

7. **Update tracker.**
   - Update any related tracker entries (`mcp__tracker__update`) — mark bugs as done, add notes describing what was done.
   Include the same content as in the "summary" you provide when the task is done.
   - If you noticed any issues during the session that haven't been filed, file them now with `mcp__tracker__create`.

8. **Push.**
   - `git push` (or `git push -u origin <branch>` if on a new branch).
   - Never force-push to master without explicit approval.

9. **Mark done.**
   - Update the session name: `/rename DONE <ID> <short-title>`.
