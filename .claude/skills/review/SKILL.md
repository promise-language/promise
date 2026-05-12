---
name: review
description: Meticulous code review of recent changes. Checks correctness, tests, conventions, and files bugs for any issues found.
---

Review the most recent changes in this project. If $ARGUMENTS is provided, it may be a tracker ID (e.g., `B0042`, `T0015`) — fetch it with `mcp__tracker__get` to determine scope — or a direct target: a Go package path (e.g., `./internal/codegen/`), a Promise test directory (e.g., `tests/e2e/`), or a specific source file to focus on. 

## Tracker status

Before starting, update your tracker status: call `mcp__tracker__heartbeat` with `status: "reviewing"` (keep the same agent, item_id, item_title from earlier phases). If you haven't registered yet, determine your agent name via `basename $(pwd)` and hostname via `hostname`.

## Steps

1. **Identify what to review.** If arguments were given, use them. Otherwise, run `git diff HEAD~1` and `git diff --cached` to find recent changes. Read every changed file in full to understand context.

2. **Check correctness.**
   - Look for logic errors, off-by-one mistakes, missing edge cases, race conditions.
   - Verify error handling: failable functions (`!`) must be propagated or handled, not silently ignored.
   - Check ownership: moves (`~`), borrows (`&`), drop flags cleared at move sites, scope cleanup ordering.
   - For codegen changes: verify LLVM IR shape (correct types, no dangling references, proper null checks).
   - For sema changes: verify all 4 passes (Declare, Define, Check, Verify) are consistent.
   - For scheduler changes: verify park mutex protocol, lock ordering, and shutdown invariants.
   - **Critical systemic checks** (these are silent — no test will catch them):
     - **Memory management** (ZERO TOLERANCE — the repo has 0 leaks): Every type that heap-allocates (native types, types with `pal_alloc`/`malloc`) must have a `drop()` path. Check: does the type have `drop(~this)`? If it has fields with droppable types, does the compiler auto-synthesize drop? If not, file a bug. **Any change that introduces even a single memory leak is blocked from being pushed.** There are NO preexisting leaks — every leak you see is a regression. File bugs for any leak sources discovered.
     - **Concurrency safety**: Shared mutable state must be mutex-protected. Channel operations must follow the park mutex protocol. Lock acquisition must follow address-ordered lock discipline. Shutdown must join all threads.
     - **Resource lifecycle**: Every `pal_alloc` must have a corresponding `pal_free` path. Every opened file/socket must be closeable. Every spawned goroutine must be reachable by shutdown.
     - **Performance traps**: Allocations inside loops that could be hoisted. Quadratic algorithms on collections. Redundant copies of large data.
   - These issues are **critical priority** bugs. File them immediately — don't defer.

3. **Check tests.**
   - Every behavioral change needs a test. Prefer batch tests (`` `test ``) over snapshot tests.
   - For Go changes: check that `codegen_test.go`, `sema_test.go`, or `ownership_test.go` cover the change.
   - For Promise changes: check for corresponding `*_test.pr` files.
   - Run `bin/verify --wasm` to confirm everything passes.
   - **Memory leak check (ZERO TOLERANCE)**: After verify completes, check the output for leak counts. **The repo has 0 leaks. Any leak in the output is a regression caused by the changes under review.** Do NOT treat any leak as preexisting — there are none. Flag any leaks as a blocking issue. Changes that introduce memory leaks will not be pushed.

4. **Check conventions.**
   - Public Promise APIs: full English words (never abbreviated), getters for side-effect-free parameterless access, `` `doc `` annotations on `` `public `` declarations.
   - No hidden effects, implicit behaviors, or action-at-a-distance.
   - No workarounds for compiler/language bugs (file a tracker bug instead).
   - Code comments reference tracker IDs where applicable.
   - **No `allow_leaks: true` tags — ever.** The repo has 0 `allow_leaks` tags. Never add `allow_leaks: true` to any test. If a test leaks, fix the leak. There are no exceptions. If you see `allow_leaks` tags in the diff, flag this as a blocking issue.

5. **Fix issues you find.** Make the corrections directly rather than just listing them.

6. **File bugs.** For issues you cannot fix (compiler limitations, design questions, flaky tests), create a tracker entry using `mcp__tracker__create` with appropriate type (`bug`, `task`, or `deferred`), priority, tags, and a clear description.

7. **Report.** Give a concise summary of what you reviewed, what you fixed, and what you filed.
