---
name: do
description: Implement the given task or fix the given bug. Once implemented or fixed follow /review /coverage then verify, rebase, commit, and mark done.
---

Implement the task or fix the bug described in $ARGUMENTS. If $ARGUMENTS references a tracker ID (e.g., `B0042`, `T0015`), fetch it with `mcp__tracker__get` first. If $ARGUMENTS is a natural-language description, use it directly.

## Steps

1. **Sync to latest.**
   - Run `git status` to check for uncommitted changes.
   - If the working tree is clean (no staged, unstaged, or untracked changes), run `git pull` to start from the most up-to-date source code.
   - If there are existing changes, skip this step — do not pull.

2. **Claim the work.**
   - If a tracker ID was given, add a note with the hostname and the repo root so other agents/hosts can see which machine is working on it.
   - The heartbeat hook automatically tracks which agent is working on which item.

3. **Plan.**
   - Run `/plan` with the same arguments. This reads the code, produces an implementation plan, and checks feasibility.
   - If `/plan` determined the item is blocked or not feasible, **stop here**.

4. **Implement.**
   - Make the code changes. Keep changes minimal and focused — don't refactor, add features, or clean up code beyond what's needed.
   - For compiler changes: update both the implementation and any affected LLVM IR patterns.
   - For Promise/stdlib changes: run `bin/build` to re-embed updated modules.
   - Never work around compiler/language bugs — if you hit a limitation, file it with `mcp__tracker__create` and stop.
   - **Proactively check for critical systemic issues** in code you touch or read during implementation. These are silent bugs that won't show up as test failures:
     - **Memory leaks** (ZERO TOLERANCE — the repo has 0 leaks and 0 `allow_leaks` tags): Does every heap-allocating type (`native` types using `pal_alloc`, types with pointer fields) have a `drop()` method or get auto-synthesized drop? Are all allocations reachable by the cleanup path? **Any change that introduces even a single memory leak is blocked from being pushed.** There are NO preexisting leaks — every leak you see is a regression caused by your changes. Fix it before proceeding. Never add `allow_leaks: true` to any test.
     - **Missing cleanup**: Are `use` bindings properly closed? Are scope bindings registered for all droppable values?
     - **Concurrency bugs**: Lock ordering violations, missing mutex protection, park/wake races, channel close races.
     - **Resource waste**: Unnecessary allocations in hot paths, O(n) operations that should be O(1), repeated work that could be cached.
   - If you spot any of these, file immediately with `mcp__tracker__create` at **critical** priority — don't wait until the end of the task. These bugs compound silently.

5. **Write tests.**
   - Every behavioral change needs tests. Write them alongside the implementation, not as an afterthought.
   - **Go tests**: `generateIR()` + `assertContains` for codegen, `checkErrs()` + `expectError` for sema, `ownerOK()` / `ownerErrs()` for ownership.
   - **Promise tests**: batch tests (`` `test `` + `assert()`) unless testing exact output. Co-locate `*_test.pr` with source for modules; use `tests/` for cross-cutting e2e tests.
   - Cover the fix/feature, edge cases, and error paths.

6. **Verify.**
   - Run the specific new tests first to confirm they pass.
   - Then run `bin/verify --local --wasm` to confirm nothing else broke.
   - If verify fails, fix the issues and re-run until green.
   - **Memory leak check (ZERO TOLERANCE)**: After verify completes, check the output for leak counts (lines like `N leaked` in test summaries). **The repo has 0 leaks. Any leak in the output is a regression caused by your changes.** Do NOT treat any leak as preexisting — there are none. Fix all leaks before proceeding. Changes that introduce memory leaks will not be pushed.
   - **No `allow_leaks: true` tags — ever.** The repo has 0 `allow_leaks` tags. Never add `allow_leaks: true` to any test. If a test leaks, fix the leak. There are no exceptions.

7. **Review, test, and commit.**
   - Run `/review` to check your changes for correctness and convention compliance.
   - Run `/coverage` scoped to the changed code to verify test coverage is adequate.
   - Run `bin/verify --local --wasm` to confirm everything passes.
   - Fetch and rebase: `git fetch origin && git rebase origin/master`.
   - If there are rebase conflicts, resolve them.
   - Run `bin/verify --local --wasm` again to confirm the rebase didn't introduce issues. (No-op rebuilds complete in ~3s.)
   - Stage and commit with a concise message including the item ID.
   - **Do NOT push.** The orchestrator handles pushing after independent verification.
   - Mark the task as done: call `mcp__tracker__update` with `status: "done"` and a `summary`.
   - Call `mcp__tracker__heartbeat` with `status: "done"`.
