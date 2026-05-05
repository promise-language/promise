---
name: do
description: Implement the given tasks or fix the given bug. Once implemented or fixed follow /review /coverage /commit skills
---

Implement the task or fix the bug described in $ARGUMENTS. If $ARGUMENTS references a tracker ID (e.g., `B0042`, `T0015`), fetch it with `mcp__tracker__get` first. If $ARGUMENTS is a natural-language description, use it directly.

## Steps

1. **Sync to latest.**
   - Run `git status` to check for uncommitted changes.
   - If the working tree is clean (no staged, unstaged, or untracked changes), run `git pull` to start from the most up-to-date source code.
   - If there are existing changes, skip this step — do not pull.

2. **Understand the problem.**
   - If a tracker ID is given, fetch it and read the full description, notes, and any linked context.
   - Read the relevant source files to understand current behavior. For compiler changes, trace the pipeline stage (parser → sema → ownership → codegen). For Promise/stdlib changes, read the module source and its tests.
   - If the problem is a bug, reproduce it first: write a minimal test case or run the failing command to confirm the issue.

3. **Plan the fix or implementation.**
   - Identify which files need to change and what the change is. Prefer the simplest approach that solves the problem.
   - For compiler changes: determine which pipeline stage(s) are involved (sema pass number, codegen phase, ownership rule, etc.).
   - For Promise code: follow the implementation philosophy — prefer Promise over IR, full English words in APIs, getters for side-effect-free parameterless access, `doc` annotations on `public` declarations.
   - If the change is non-trivial, briefly state the plan before starting.

4. **Claim the work.**
   - If a tracker ID was given, update its status to `in_progress` using `mcp__tracker__update`.
   - Add a note with the hostname (`hostname` command) and the repo root (`pwd`) so other agents/hosts can see which machine is working on it.

5. **Implement.**
   - Make the code changes. Keep changes minimal and focused — don't refactor, add features, or clean up code beyond what's needed.
   - For compiler changes: update both the implementation and any affected LLVM IR patterns.
   - For Promise/stdlib changes: run `./build` to re-embed updated modules.
   - Never work around compiler/language bugs — if you hit a limitation, file it with `mcp__tracker__create` and stop.

6. **Write tests.**
   - Every behavioral change needs tests. Write them alongside the implementation, not as an afterthought.
   - **Go tests**: `generateIR()` + `assertContains` for codegen, `checkErrs()` + `expectError` for sema, `ownerOK()` / `ownerErrs()` for ownership.
   - **Promise tests**: batch tests (`` `test `` + `assert()`) unless testing exact output. Co-locate `*_test.pr` with source for modules; use `tests/` for cross-cutting e2e tests.
   - Cover the fix/feature, edge cases, and error paths.

7. **Verify.**
   - Run the specific new tests first to confirm they pass.
   - Then run `bin/verify.sh --local --wasm` to confirm nothing else broke.
   - If verify fails, fix the issues and re-run until green.

8. **Chain to /review, /coverage, /commit.**
   - Run `/review` to check your own changes for correctness, missed edge cases, and convention compliance.
   - Run `/coverage` scoped to the changed code to verify test coverage is adequate.
   - Run `/commit` to verify, commit, sync, and push (which handles tracker updates before pushing).
