---
name: do
description: Implement the given tasks or fix the given bug. Once implemented or fixed follow /review /coverage then verify, rebase, commit, and mark done.
---

Implement the task or fix the bug described in $ARGUMENTS. If $ARGUMENTS references a tracker ID (e.g., `B0042`, `T0015`), fetch it with `mcp__tracker__get` first. If $ARGUMENTS is a natural-language description, use it directly.

## Steps

1. **Register with tracker.**
   - Determine your agent name by running `basename $(pwd)` (e.g., `linux_promise_3`).
   - Determine your hostname by running `hostname`.
   - Call `mcp__tracker__heartbeat` with `agent: <dirname>`, `host: <hostname>`, `status: "planning"`, `item_id: "<ID>"`, `item_title: "<short-title>"`. Do this before any other work.

2. **Sync to latest.**
   - Run `git status` to check for uncommitted changes.
   - If the working tree is clean (no staged, unstaged, or untracked changes), run `git pull` to start from the most up-to-date source code.
   - If there are existing changes, skip this step — do not pull.

3. **Claim the work.**
   - If a tracker ID was given, update its status to `in_progress` and set `assigned_to` to your agent name using `mcp__tracker__update`.
   - Add a note with the hostname and the repo root so other agents/hosts can see which machine is working on it.

4. **Understand the problem.**
   - If a tracker ID is given, fetch it and read the full description, notes, and any linked context.
   - Read the relevant source files to understand current behavior. For compiler changes, trace the pipeline stage (parser → sema → ownership → codegen). For Promise/stdlib changes, read the module source and its tests.
   - If the problem is a bug, reproduce it first: write a minimal test case or run the failing command to confirm the issue.

5. **Plan the fix or implementation.**
   - Identify which files need to change and what the change is. Prefer the simplest approach that solves the problem.
   - For compiler changes: determine which pipeline stage(s) are involved (sema pass number, codegen phase, ownership rule, etc.).
   - For Promise code: follow the implementation philosophy — prefer Promise over IR, full English words in APIs, getters for side-effect-free parameterless access, `doc` annotations on `public` declarations.
   - If the change is non-trivial, briefly state the plan before starting.

6. **Evaluate feasibility.**
   After planning, determine whether the item can actually be implemented right now. There are three possible outcomes:

   **A) Blocked** — the item depends on work that hasn't been done yet:
   - Identify the blocking item. If one doesn't exist in the tracker, create it with `mcp__tracker__create`.
   - Set `blocked_by` on the current item to reference the blocker(s) using `mcp__tracker__update`.
   - Add a note to the current item explaining the dependency and why it can't proceed now.
   - Add the `needs-attention` tag to the current item (append to existing tags) so the user knows to review it.
   - Set the item status back to `open` (blocking is computed from `blocked_by`, not from status).
   - Call `mcp__tracker__heartbeat` with `status: "done"`.
   - **Stop here.** Do not proceed to implementation.

   **B) Not feasible** — the item cannot be done as specified (wrong assumptions, contradictory requirements, out of scope, etc.):
   - Set the item status to `wontfix` using `mcp__tracker__update`.
   - Provide a clear `summary` explaining why it's not feasible and what would need to change.
   - Add a note with the detailed reasoning.
   - Add the `needs-attention` tag to the current item (append to existing tags) so the user knows to review it.
   - Call `mcp__tracker__heartbeat` with `status: "done"`.
   - **Stop here.** Do not proceed to implementation.

   **C) Feasible** — proceed to step 7.

7. **Implement.**
   - Update your tracker status: call `mcp__tracker__heartbeat` with `status: "implementing"` (keep the same agent, item_id, item_title).
   - Make the code changes. Keep changes minimal and focused — don't refactor, add features, or clean up code beyond what's needed.
   - For compiler changes: update both the implementation and any affected LLVM IR patterns.
   - For Promise/stdlib changes: run `./build` to re-embed updated modules.
   - Never work around compiler/language bugs — if you hit a limitation, file it with `mcp__tracker__create` and stop.
   - **Proactively check for critical systemic issues** in code you touch or read during implementation. These are silent bugs that won't show up as test failures:
     - **Memory leaks** (ZERO TOLERANCE — the repo has 0 leaks and 0 `allow_leaks` tags): Does every heap-allocating type (`native` types using `pal_alloc`, types with pointer fields) have a `drop()` method or get auto-synthesized drop? Are all allocations reachable by the cleanup path? **Any change that introduces even a single memory leak is blocked from being pushed.** There are NO preexisting leaks — every leak you see is a regression caused by your changes. Fix it before proceeding. Never add `allow_leaks: true` to any test.
     - **Missing cleanup**: Are `use` bindings properly closed? Are scope bindings registered for all droppable values?
     - **Concurrency bugs**: Lock ordering violations, missing mutex protection, park/wake races, channel close races.
     - **Resource waste**: Unnecessary allocations in hot paths, O(n) operations that should be O(1), repeated work that could be cached.
   - If you spot any of these, file immediately with `mcp__tracker__create` at **critical** priority — don't wait until the end of the task. These bugs compound silently.

8. **Write tests.**
   - Every behavioral change needs tests. Write them alongside the implementation, not as an afterthought.
   - **Go tests**: `generateIR()` + `assertContains` for codegen, `checkErrs()` + `expectError` for sema, `ownerOK()` / `ownerErrs()` for ownership.
   - **Promise tests**: batch tests (`` `test `` + `assert()`) unless testing exact output. Co-locate `*_test.pr` with source for modules; use `tests/` for cross-cutting e2e tests.
   - Cover the fix/feature, edge cases, and error paths.

9. **Verify.**
   - Run the specific new tests first to confirm they pass.
   - Then run `bin/verify.sh --local --wasm` (Linux/macOS) or `powershell -ExecutionPolicy Bypass -File bin\verify.ps1 -Local` (Windows) to confirm nothing else broke.
   - If verify fails, fix the issues and re-run until green.
   - **Memory leak check (ZERO TOLERANCE)**: After verify completes, check the output for leak counts (lines like `N leaked` in test summaries). **The repo has 0 leaks. Any leak in the output is a regression caused by your changes.** Do NOT treat any leak as preexisting — there are none. Fix all leaks before proceeding. Changes that introduce memory leaks will not be pushed.
   - **No `allow_leaks: true` tags — ever.** The repo has 0 `allow_leaks` tags. Never add `allow_leaks: true` to any test. If a test leaks, fix the leak. There are no exceptions.

10. **Review, test, and commit.**
   - Run `/review` to check your changes for correctness and convention compliance.
   - Run `/coverage` scoped to the changed code to verify test coverage is adequate.
   - Run `bin/verify.sh --local --wasm` to confirm everything passes.
   - Fetch and rebase: `git fetch origin && git rebase origin/master`.
   - If there are rebase conflicts, resolve them.
   - Run `bin/verify.sh --local --wasm` again to confirm the rebase didn't introduce issues. (No-op rebuilds complete in ~3s.)
   - Stage and commit with a concise message including the item ID.
   - **Do NOT push.** The orchestrator handles pushing after independent verification.
   - Mark the task as done: call `mcp__tracker__update` with `status: "done"` and a `summary`.
   - Call `mcp__tracker__heartbeat` with `status: "done"`.
