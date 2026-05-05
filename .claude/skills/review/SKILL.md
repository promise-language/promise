---
name: review
description: Meticulous code review of recent changes. Checks correctness, tests, conventions, and files bugs for any issues found.
---

Review the most recent changes in this project. If $ARGUMENTS is provided, treat it as a file path, directory, commit range, or PR number to scope the review.

## Steps

1. **Identify what to review.** If arguments were given, use them. Otherwise, run `git diff HEAD~1` and `git diff --cached` to find recent changes. Read every changed file in full to understand context.

2. **Check correctness.**
   - Look for logic errors, off-by-one mistakes, missing edge cases, race conditions.
   - Verify error handling: failable functions (`!`) must be propagated or handled, not silently ignored.
   - Check ownership: moves (`~`), borrows (`&`), drop flags cleared at move sites, scope cleanup ordering.
   - For codegen changes: verify LLVM IR shape (correct types, no dangling references, proper null checks).
   - For sema changes: verify all 4 passes (Declare, Define, Check, Verify) are consistent.
   - For scheduler changes: verify park mutex protocol, lock ordering, and shutdown invariants.

3. **Check tests.**
   - Every behavioral change needs a test. Prefer batch tests (`` `test ``) over snapshot tests.
   - For Go changes: check that `codegen_test.go`, `sema_test.go`, or `ownership_test.go` cover the change.
   - For Promise changes: check for corresponding `*_test.pr` files.
   - Run `bin/verify.sh --local --wasm` to confirm everything passes.

4. **Check conventions.**
   - Public Promise APIs: full English words (never abbreviated), getters for side-effect-free parameterless access, `` `doc `` annotations on `` `public `` declarations.
   - No hidden effects, implicit behaviors, or action-at-a-distance.
   - No workarounds for compiler/language bugs (file a tracker bug instead).
   - Code comments reference tracker IDs where applicable.

5. **Fix issues you find.** Make the corrections directly rather than just listing them.

6. **File bugs.** For issues you cannot fix (compiler limitations, design questions, flaky tests), create a tracker entry using `mcp__tracker__create` with appropriate type (`bug`, `task`, or `deferred`), priority, tags, and a clear description.

7. **Report.** Give a concise summary of what you reviewed, what you fixed, and what you filed.
