---
name: plan
description: Understand the problem and produce an implementation plan. Signals the transition from planning to implementing.
---

Understand the problem described in $ARGUMENTS and produce a concrete implementation plan. If $ARGUMENTS references a tracker ID (e.g., `B0042`, `T0015`), fetch it with `mcp__tracker__get` first.

## Steps

1. **Understand the problem.**
   - If a tracker ID is given, fetch it and read the full description, notes, and any linked context.
   - Read the relevant source files to understand current behavior. For compiler changes, trace the pipeline stage (parser -> sema -> ownership -> codegen). For Promise/stdlib changes, read the module source and its tests.
   - If the problem is a bug, reproduce it first: write a minimal test case or run the failing command to confirm the issue.

2. **Plan the fix or implementation.**
   - Identify which files need to change and what the change is. Prefer the simplest approach that solves the problem.
   - For compiler changes: determine which pipeline stage(s) are involved (sema pass number, codegen phase, ownership rule, etc.).
   - For Promise code: follow the implementation philosophy -- prefer Promise over IR, full English words in APIs, getters for side-effect-free parameterless access, `doc` annotations on `public` declarations.
   - State the plan clearly before returning.

3. **Evaluate feasibility.**
   Determine whether the item can actually be implemented right now. Three outcomes:

   **A) Blocked** -- depends on work that hasn't been done yet:
   - Identify the blocking item. If one doesn't exist, create it with `mcp__tracker__create`.
   - Set `blocked_by` on the current item using `mcp__tracker__update`.
   - Add a note explaining the dependency.
   - Add the `needs-attention` tag (append to existing tags) so the user knows to review it.
   - Set the item status back to `open`.
   - **Stop here.**

   **B) Not feasible** -- cannot be done as specified:
   - Set the item status to `wontfix` using `mcp__tracker__update`.
   - Provide a clear `summary` explaining why.
   - Add a note with the detailed reasoning.
   - Add the `needs-attention` tag (append to existing tags) so the user knows to review it.
   - **Stop here.**

   **C) Feasible** -- return the plan. Implementation begins after this skill exits.
