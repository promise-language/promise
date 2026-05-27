---
name: implement
description: Implement the plan produced by /plan. Makes code changes, writes tests, and verifies the build passes.
---

Implement the task or fix the bug described in $ARGUMENTS. If $ARGUMENTS references a tracker ID (e.g., `B0042`, `T0015`), fetch it with `mcp__tracker__get` first and read the `plan` field for the implementation plan.

## Steps

1. **Implement.**
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

2. **Write tests.**
   - Every behavioral change needs tests. Write them alongside the implementation, not as an afterthought.
   - **Go tests**: `generateIR()` + `assertContains` for codegen, `checkErrs()` + `expectError` for sema, `ownerOK()` / `ownerErrs()` for ownership.
   - **Promise tests**: batch tests (`` `test `` + `assert()`) unless testing exact output. Co-locate `*_test.pr` with source for modules; use `tests/` for cross-cutting e2e tests.
   - Cover the fix/feature, edge cases, and error paths.

3. **Verify — run it in the FOREGROUND, never in the background.**
   - **CRITICAL: run verify as a normal blocking command and wait for it to finish in this same turn.** Do NOT launch it with `run_in_background`, a trailing `&`, `setsid`, `nohup`, or as a harness background task, and do NOT end your turn expecting to be "re-invoked automatically when it completes." There is no auto-re-invocation here: this skill runs as a single orchestration step (one Turn). If you yield while verify is still running, the step ends with the task incomplete, the orchestrator re-runs `implement` from a **fresh session** (your in-progress, uncommitted work is stranded in a now-dirty worktree), and after the step's `max_runs` is hit the task is **paused** with `step "implement" exceeded max runs`. Block on verify and wait — it can take many minutes; that is expected and fine.
   - Run the specific new tests first to confirm they pass.
   - Then run `bin/verify --wasm` to confirm nothing else broke. Wait for it to return inline before doing anything else.
   - If verify fails, fix the issues and re-run until green.
   - **Memory leak check (ZERO TOLERANCE)**: After verify completes, check the output for leak counts (lines like `N leaked` in test summaries). **The repo has 0 leaks. Any leak in the output is a regression caused by your changes.** Do NOT treat any leak as preexisting — there are none. Fix all leaks before proceeding. Changes that introduce memory leaks will not be pushed.
   - **No `allow_leaks: true` tags — ever.** The repo has 0 `allow_leaks` tags. Never add `allow_leaks: true` to any test. If a test leaks, fix the leak. There are no exceptions.

4. **Mark done — do not end your turn before this.**
   - Only after verify is green, call `mcp__tracker__update` with `status: "done"` and a `summary` of what was changed.
   - **Your turn is not finished until you have either marked the task done or filed a blocker and stopped.** Never end the turn while a build, verify, or commit is still in flight — finish it inline first. Leaving the task `open` at turn end just makes the orchestrator re-run this step from scratch.
