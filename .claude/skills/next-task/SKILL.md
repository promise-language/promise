---
name: next-task
description: Select the next open task to work on. If tags are given, focus on tasks with those tags. Do your best to select the optimal next task from all open tasks. To minimize merge conflicts, avoid items sharing similar tags with other "in-progress" items. Items tagged with a platform name (like windows, posix, linux) can only be worked on if the host matches the platform. After selecting the task chain to /do.
---

Select the highest-impact, most-actionable open task and chain to `/do` to implement it.

## Inputs

- `$ARGUMENTS` — optional space-separated **tags** (e.g., `codegen`, `sema`, `scheduler`) to narrow the search. If empty, consider all open tasks.

## Steps

### 1. Check host platform

- Run `uname -s` and `uname -m` to determine the current platform.
- Platform tags to check: `windows`, `posix`, `linux`, `darwin`. Tasks tagged with a platform can only be worked on if the host matches. Any host can work on `wasm` items.

### 2. Gather open tasks

- Call `mcp__tracker__list` with `type: "task"`, `status: "open"`. If `$ARGUMENTS` contains a single tag, pass it as the `tag` filter.
- If `$ARGUMENTS` contains multiple tags, fetch without a tag filter and manually filter to tasks that match **any** of the given tags.
- **Filter out platform-incompatible tasks**: skip any task tagged with a platform that doesn't match the current host.
- If no open tasks match, also check `status: "in_progress"` tasks that may be stale (no recent notes, not claimed by another host). If still nothing, tell the user there are no actionable tasks and stop.

### 3. Check in-progress items for conflict avoidance

- Call `mcp__tracker__list` with `status: "in_progress"` (no type filter) to see what's currently being worked on.
- Note the tags of in-progress items. When ranking candidates, deprioritize tasks whose tags overlap heavily with in-progress items — working on the same subsystem concurrently increases merge conflict risk.

### 4. Rank candidates

Score each task using these criteria (in priority order — earlier criteria dominate):

1. **Priority** — `critical` > `high` > `medium` > `low`.
2. **Blocker potential** — tasks whose description or notes mention other tracker IDs they block (e.g., "blocks T0012") rank higher. Tasks that are themselves blocked by other open items rank lower.
3. **Conflict risk** — deprioritize tasks with tags that overlap with in-progress items (from step 3).
4. **Actionability** — prefer tasks with clear specifications, acceptance criteria, or identified implementation approaches. Deprioritize tasks described vaguely.
5. **Scope** — prefer smaller, well-scoped tasks over large uncertain ones.
6. **Recency** — among otherwise equal candidates, prefer older tasks (longer outstanding).

### 5. Read top candidates

- For the top 3–5 candidates, call `mcp__tracker__get` to read full details (description, notes, linked context).
- Skim the relevant source area briefly to confirm the task is still needed and the implementation seems feasible.
- If a task turns out to be already done or no longer relevant, update it via `mcp__tracker__update` (status: `done` or `wontfix` with a note) and move to the next candidate.

### 6. Select and explain

- Pick the single best task to work on.
- State your selection concisely: the task ID, title, a one-sentence rationale for why this is the optimal next task (e.g., "T0015 is high priority, well-specified, and unblocks B0042").

### 7. Chain to /do

- Invoke `/do <task-id>` to implement the task.

## Selection examples

| Scenario | Outcome |
|----------|---------|
| One critical task, several medium | Pick the critical task |
| Two high-priority tasks, one blocks other items | Pick the blocker |
| All medium priority, one has a clear spec in notes | Pick the one with the clear spec |
| User passes `codegen scheduler` | Only consider tasks tagged `codegen` or `scheduler` |
| Task tagged `windows` but host is `darwin` | Skip it |
| Task tagged `codegen`, another `codegen` item is in-progress | Deprioritize to reduce conflict risk |
| No open tasks remain | Report "No open tasks to work on" and stop |
