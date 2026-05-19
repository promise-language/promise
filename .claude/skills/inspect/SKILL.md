---
name: inspect
description: Independent inspection of completed work. Assesses completion, quality, and test coverage.
---

Inspect the implementation of item $ARGUMENTS. You are providing an independent assessment — the implementing agent already reviewed their own work; your job is to verify their claims.

## Steps

1. **Fetch the item.**
   - Call `mcp__tracker__get` with the item ID from $ARGUMENTS.
   - Read the full item: description, summary, notes, and chat_log.
   - The **description** tells you what was requested. The **summary** is the implementing agent's self-report. The **chat_log** shows their reasoning. Your job is to verify independently.

2. **Find the relevant changes.**
   - Run `git log --oneline --all --grep="<ID>" -20` to find commits mentioning this item.
   - If no commits found, run `git log --oneline -20` and look for relevant recent commits.
   - Run `git diff <before>..<after>` to see all code changes for this item.
   - If the commit range is unclear, use `git show <commit>` on individual commits.

3. **Read the changed files.**
   - Read every file that was modified, in full — not just the diff hunks.
   - Understand the implementation in context of the surrounding code.

4. **Assess completion.**
   - Does the implementation actually address what was requested in the description?
   - Are all requirements met? Check each point in the description.
   - Were corners cut? Did the agent work around issues instead of solving them?
   - Is the task genuinely done, or is it partially done with the summary claiming otherwise?

5. **Assess quality.**
   - Is the code well-structured and consistent with the rest of the codebase?
   - **Compiler changes (Go):**
     - Correct LLVM IR shape: proper types, no dangling references, null checks where needed.
     - Sema consistency across all 4 passes (Declare, Define, Check, Verify).
     - Ownership: moves (`~`), borrows (`&`), drop flags cleared at move sites, scope cleanup ordering.
     - Scheduler invariants: park mutex protocol, lock ordering, shutdown correctness.
   - **Promise/stdlib changes:**
     - Conventions: full English words in APIs (with approved abbreviations), getters for side-effect-free parameterless access, `` `doc `` annotations on `` `public `` declarations.
     - No hidden effects, implicit behaviors, or action-at-a-distance.
     - No workarounds for compiler/language bugs (should be filed as tracker bugs instead).
   - **Memory management** (ZERO TOLERANCE — the repo has 0 leaks):
     - Every heap-allocating type must have a `drop()` path.
     - No `allow_leaks: true` tags — ever. The repo has 0 such tags.
     - **Any change that introduces even a single memory leak is blocked from being pushed.** There are NO preexisting leaks — every leak is a regression.
   - **Concurrency safety:** shared mutable state mutex-protected, channel operations follow park mutex protocol, address-ordered lock discipline.
   - Are there bugs, race conditions, or edge cases missed?

6. **Assess test coverage.**
   - Were tests added or updated for the new/changed functionality?
   - **Go tests:** `codegen_test.go` (IR shape via `generateIR` + `assertContains`), `sema_test.go` (errors via `checkErrs` + `expectError`), `ownership_test.go` (via `ownerOK`/`ownerErrs`).
   - **Promise tests:** batch tests (`` `test `` + `assert()`) preferred over snapshot tests. Co-located `*_test.pr` for modules, `tests/` for cross-cutting e2e.
   - Do the tests cover the happy path, edge cases, and error paths?
   - Are there obvious gaps in test coverage?

7. **Write the inspection result.**
   - Call `mcp__tracker__update` with `id: "<ID>"` and `inspection` set to a JSON string:
     ```json
     {
       "inspected_at": "<current UTC ISO timestamp>",
       "inspected_by": "<your agent name>",
       "complete": true or false,
       "quality": "good" or "acceptable" or "poor",
       "completeness": "full" or "partial" or "minimal",
       "test_coverage": "adequate" or "insufficient" or "none",
       "verdict": "pass" or "concerns" or "fail",
       "summary": "2-3 sentence markdown summary of your findings"
     }
     ```
   - Also add a note to the item: `note: "Inspection: <verdict>. <one-line summary>"`, `agent: "<your agent name>"`.

## Rules

- **You are reviewing someone else's work. Be objective and thorough.**
- **Do NOT make any code changes.** This is a read-only inspection.
- **Do NOT run build or tests.** The verify step already handled that before push. Focus on reading code.
- **Focus on substance, not style.** Don't flag cosmetic issues. Focus on whether the work actually solves the problem.
- **Compare claims to reality.** If the implementing agent's summary claims something was done but the code tells a different story, flag it clearly.
- **Read the chat log** to understand the agent's reasoning, but form your own assessment of the code.
- **Be specific.** Reference file names and line numbers in your findings. Vague assessments are not useful.
