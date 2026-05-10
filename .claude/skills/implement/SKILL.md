---
name: implement
description: Implement the plan produced by /plan. Makes code changes, writes tests, and verifies the build passes.
---

Implement the task or fix the bug described in $ARGUMENTS. If $ARGUMENTS references a tracker ID (e.g., `B0042`, `T0015`), fetch it with `mcp__tracker__get` first. A plan should already exist from a prior `/plan` step — read the task notes to find it.

## Steps

1. **Implement.**
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

2. **Write tests.**
   - Every behavioral change needs tests. Write them alongside the implementation, not as an afterthought.
   - **Go tests**: `generateIR()` + `assertContains` for codegen, `checkErrs()` + `expectError` for sema, `ownerOK()` / `ownerErrs()` for ownership.
   - **Promise tests**: batch tests (`` `test `` + `assert()`) unless testing exact output. Co-locate `*_test.pr` with source for modules; use `tests/` for cross-cutting e2e tests.
   - Cover the fix/feature, edge cases, and error paths.

3. **Verify.**
   - Run the specific new tests first to confirm they pass.
   - Then run `bin/verify.sh --local --wasm` (Linux/macOS) or `powershell -ExecutionPolicy Bypass -File bin\verify.ps1 -Local` (Windows) to confirm nothing else broke.
   - If verify fails, fix the issues and re-run until green.
   - **Memory leak check (ZERO TOLERANCE)**: After verify completes, check the output for leak counts (lines like `N leaked` in test summaries). **The repo has 0 leaks. Any leak in the output is a regression caused by your changes.** Do NOT treat any leak as preexisting — there are none. Fix all leaks before proceeding. Changes that introduce memory leaks will not be pushed.
   - **No `allow_leaks: true` tags — ever.** The repo has 0 `allow_leaks` tags. Never add `allow_leaks: true` to any test. If a test leaks, fix the leak. There are no exceptions.

4. **Mark done.**
   - Call `mcp__tracker__update` with `status: "done"` and a `summary` of what was changed.
   - Call `mcp__tracker__heartbeat` with `status: "done"`.
