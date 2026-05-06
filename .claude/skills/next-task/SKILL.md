---
name: next-task
description: Select the next open task to fix. If tags are given, focus on the tasks with this tags. Do your best to select the optimal next task to work on from all open tasks. To minimize merge conflicts, avoid items sharing similar thats with other "in-progress" items. Items tagged with platform name (like windows, poxis, linux) can only be worked on if the host matches the platform. After selecting the task to work on chain to /do.
---

Select the highest-impact, most-actionable open bug and chain to `/do` to fix it.

## Inputs

- `$ARGUMENTS` — optional space-separated **tags** (e.g., `codegen`, `sema`, `scheduler`) to narrow the search. If empty, consider all open bugs.

## Steps

### 1. Gather open bugs

- Call `mcp__tracker__list` with `type: "bug"`, `status: "open"`. If `$ARGUMENTS` contains a single tag, pass it as the `tag` filter.
- If `$ARGUMENTS` contains multiple tags, fetch without a tag filter and manually filter to bugs that match **any** of the given tags.
- If no open bugs match, also check `status: "in_progress"` bugs that may be stale (no recent notes, not claimed by another host). If still nothing, tell the user there are no actionable bugs and stop.

### 2. Rank candidates

Score each bug using these criteria (in priority order — earlier criteria dominate):

1. **Priority** — `critical` > `high` > `medium` > `low`.
2. **Blocker potential** — bugs whose description or notes mention other tracker IDs they block (e.g., "blocks T0012") rank higher. Bugs that are themselves blocked by other open bugs rank lower.
3. **Actionability** — prefer bugs with clear reproduction steps, specific error messages, or identified root causes. Deprioritize bugs described vaguely or tagged with unrelated/unknown subsystems.
4. **Scope** — prefer smaller, well-scoped bugs over large uncertain ones. A focused codegen fix beats an open-ended design question.
5. **Recency** — among otherwise equal candidates, prefer older bugs (longer outstanding).

### 3. Read top candidates

- For the top 3–5 candidates, call `mcp__tracker__get` to read full details (description, notes, linked context).
- Skim the relevant source area briefly (e.g., grep for the error message or read the referenced file) to confirm the bug is still present and the fix seems feasible.
- If a bug turns out to be already fixed, stale, or not reproducible, update it via `mcp__tracker__update` (status: `cant_reproduce` or `done` with a note) and move to the next candidate.

### 4. Select and explain

- Pick the single best bug to fix.
- State your selection concisely: the bug ID, title, a one-sentence rationale for why this is the optimal next fix (e.g., "B0042 is high priority, has a clear repro, and unblocks T0015").

### 5. Chain to /do

- Invoke `/do <bug-id>` to implement the fix.

## Selection examples

| Scenario | Outcome |
|----------|---------|
| One critical bug, several medium | Pick the critical bug |
| Two high-priority bugs, one blocks a task | Pick the blocker |
| All medium priority, one has a clear root cause in notes | Pick the one with the clear root cause |
| User passes `codegen scheduler` | Only consider bugs tagged `codegen` or `scheduler` |
| No open bugs remain | Report "No open bugs to fix" and stop |
