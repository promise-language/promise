---
name: next
description: Select the next open item (task or bug) to work on. If tags are given, focus on items with those tags. Do your best to select the optimal next item from all open items. To minimize merge conflicts, avoid items sharing similar tags with other "in-progress" items. Items tagged with a platform name (like windows, posix, linux) can only be worked on if the host matches the platform. After selecting the item chain to /do.
---

Select the highest-impact, most-actionable open item (bug or task) and chain to `/do` to work on it.

## Inputs

- `$ARGUMENTS` — optional space-separated **tags** (e.g., `codegen`, `sema`, `scheduler`) to narrow the search. If empty, consider all open items.

## Steps

### 1. Check host platform

- Run `uname -s` and `uname -m` to determine the current platform.
- Platform tags to check: `windows`, `posix`, `linux`, `darwin`. Items tagged with a platform can only be worked on if the host matches. Any host can work on `wasm` items.

### 2. Gather open items

- Call `mcp__tracker__list` with `type: "bug"`, `status: "open"` to get all open bugs.
- Call `mcp__tracker__list` with `type: "task"`, `status: "open"` to get all open tasks.
- If `$ARGUMENTS` contains tags, filter to items that match **any** of the given tags.
- **Filter out platform-incompatible items**: skip any item tagged with a platform that doesn't match the current host.
- If no open items match, also check `status: "in_progress"` items (both bugs and tasks) that may be stale (no recent notes, not claimed by another host). If still nothing, tell the user there are no actionable items and stop.

### 3. Check in-progress items for conflict avoidance

- Call `mcp__tracker__list` with `status: "in_progress"` (no type filter) to see what's currently being worked on.
- Note the tags of in-progress items. When ranking candidates, deprioritize items whose tags overlap heavily with in-progress items — working on the same subsystem concurrently increases merge conflict risk.

### 4. Rank candidates

Merge bugs and tasks into a single ranked list using these criteria (in priority order — earlier criteria dominate):

1. **Priority** — `critical` > `high` > `medium` > `low`.
2. **Type tiebreak** — at equal priority, bugs rank above tasks (fixes before features).
3. **Blocker potential** — items whose description or notes mention other tracker IDs they block rank higher. Items that are themselves blocked by other open items rank lower.
4. **Conflict risk** — deprioritize items with tags that overlap with in-progress items (from step 3).
5. **Actionability** — prefer items with clear reproduction steps (bugs) or clear specs (tasks). Deprioritize vaguely described items.
6. **Scope** — prefer smaller, well-scoped items over large uncertain ones.
7. **Recency** — among otherwise equal candidates, prefer older items (longer outstanding).

### 5. Read top candidates

- For the top 3–5 candidates, call `mcp__tracker__get` to read full details (description, notes, linked context).
- Skim the relevant source area briefly to confirm the item is still actionable.
- If an item turns out to be already fixed/done, stale, or not reproducible, update it via `mcp__tracker__update` (status: `done`, `cant_reproduce`, or `wontfix` with a note) and move to the next candidate.

### 6. Select and explain

- Pick the single best item to work on.
- State your selection concisely: the item ID, title, a one-sentence rationale for why this is the optimal next item (e.g., "B0042 is high priority, has a clear repro, and unblocks T0015").

### 7. Chain to /do

- Invoke `/do <item-id>` to implement the fix or task.

## Selection examples

| Scenario | Outcome |
|----------|---------|
| One critical bug, several medium tasks | Pick the critical bug |
| High-priority task and high-priority bug, bug blocks nothing | Pick whichever is more actionable (bugs tiebreak above tasks) |
| Two high-priority bugs, one blocks a task | Pick the blocker |
| All medium priority, one bug has a clear root cause | Pick the bug with the clear root cause |
| User passes `codegen scheduler` | Only consider items tagged `codegen` or `scheduler` |
| Item tagged `linux` but host is `darwin` | Skip it |
| Task tagged `sema`, another `sema` bug is in-progress | Deprioritize to reduce conflict risk |
| No open items remain | Report "No open items to work on" and stop |
