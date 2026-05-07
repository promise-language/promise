---
name: do
description: Implement the given tasks or fix the given bug. Once implemented or fixed follow /review /coverage /commit skills
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

6. **Implement.**
   - Update your tracker status: call `mcp__tracker__heartbeat` with `status: "implementing"` (keep the same agent, item_id, item_title).
   - Make the code changes. Keep changes minimal and focused — don't refactor, add features, or clean up code beyond what's needed.
   - For compiler changes: update both the implementation and any affected LLVM IR patterns.
   - For Promise/stdlib changes: run `./build` to re-embed updated modules.
   - Never work around compiler/language bugs — if you hit a limitation, file it with `mcp__tracker__create` and stop.
   - **Proactively check for critical systemic issues** in code you touch or read during implementation. These are silent bugs that won't show up as test failures:
     - **Memory leaks** (PRIORITY — active leak-reduction phase, baseline ~2170 leaks): Does every heap-allocating type (`native` types using `pal_alloc`, types with pointer fields) have a `drop()` method or get auto-synthesized drop? Are all allocations reachable by the cleanup path? When touching any code, look for opportunities to fix leaks — even if unrelated to the current task, file bugs for any leaks discovered.
     - **Missing cleanup**: Are `use` bindings properly closed? Are scope bindings registered for all droppable values?
     - **Concurrency bugs**: Lock ordering violations, missing mutex protection, park/wake races, channel close races.
     - **Resource waste**: Unnecessary allocations in hot paths, O(n) operations that should be O(1), repeated work that could be cached.
   - If you spot any of these, file immediately with `mcp__tracker__create` at **critical** priority — don't wait until the end of the task. These bugs compound silently.

7. **Write tests.**
   - Every behavioral change needs tests. Write them alongside the implementation, not as an afterthought.
   - **Go tests**: `generateIR()` + `assertContains` for codegen, `checkErrs()` + `expectError` for sema, `ownerOK()` / `ownerErrs()` for ownership.
   - **Promise tests**: batch tests (`` `test `` + `assert()`) unless testing exact output. Co-locate `*_test.pr` with source for modules; use `tests/` for cross-cutting e2e tests.
   - Cover the fix/feature, edge cases, and error paths.

8. **Verify.**
   - Run the specific new tests first to confirm they pass.
   - Then run `bin/verify.sh --local --wasm` (Linux/macOS) or `powershell -ExecutionPolicy Bypass -File bin\verify.ps1 -Local` (Windows) to confirm nothing else broke.
   - If verify fails, fix the issues and re-run until green.
   - **Memory leak tracking**: After verify completes, check the output for leak counts (lines like `N leaked` in test summaries). Record the total leak count. Compare against the baseline (~2170 leaks). If your changes increased leaks, investigate and fix before proceeding. If your changes reduced leaks, note the improvement. We are in an active leak-reduction phase — all changes should aim to minimize memory leaks.
   - **No new `allow_leaks: true` tags.** Never add `allow_leaks: true` to tests — it masks leaks and prevents detection of regressions. If a test leaks, fix the leak or file a bug. The only allowed change to `allow_leaks` tags is **removing** them (when a test no longer leaks).

9. **Chain to /review, /coverage, /commit.**
   - Run `/review` to check your own changes for correctness, missed edge cases, and convention compliance.
   - Run `/coverage` scoped to the changed code to verify test coverage is adequate.
   - Run `/commit` to verify, commit, sync, and push.
