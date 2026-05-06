---
name: next-bug
description: Select the next open bug to fix. If tags are given, focus on bugs with those tags. Do your best to select the optimal next bug to fix from all open bugs. To minimize merge conflicts, avoid items sharing similar tags with other "in-progress" items. Items tagged with a platform name (like windows, posix, linux) can only be worked on if the host matches the platform. After selecting the bug chain to /do.
---

Select the highest-impact, most-actionable open bug and chain to `/do` to fix it.

## Inputs

- `$ARGUMENTS` — optional space-separated **tags** (e.g., `codegen`, `sema`, `scheduler`) to narrow the search. If empty, consider all open bugs.

## Steps

### 1. Check host platform

- Run `uname -s` and `uname -m` to determine the current platform.
- Platform tags to check: `windows`, `posix`, `linux`, `darwin`. Bugs tagged with a platform can only be worked on if the host matches. Any host can work on `wasm` items.

### 2. Gather open bugs

- Call `mcp__tracker__list` with `type: "bug"`, `status: "open"`. If `$ARGUMENTS` contains a single tag, pass it as the `tag` filter.
- If `$ARGUMENTS` contains multiple tags, fetch without a tag filter and manually filter to bugs that match **any** of the given tags.
- **Filter out platform-incompatible bugs**: skip any bug tagged with a platform that doesn't match the current host.
- If no open bugs match, also check `status: "in_progress"` bugs that may be stale (no recent notes, not claimed by another host). If still nothing, tell the user there are no actionable bugs and stop.

### 3. Check in-progress items for conflict avoidance

- Call `mcp__tracker__list` with `status: "in_progress"` (no type filter) to see what's currently being worked on.
- Note the tags of in-progress items. When ranking candidates, deprioritize bugs whose tags overlap heavily with in-progress items — working on the same subsystem concurrently increases merge conflict risk.

### 4. Rank candidates

Score each bug using these criteria (in priority order — earlier criteria dominate):

1. **Priority** — `critical` > `high` > `medium` > `low`.
2. **Blocker potential** — bugs whose description or notes mention other tracker IDs they block (e.g., "blocks T0012") rank higher. Bugs that are themselves blocked by other open bugs rank lower.
3. **Conflict risk** — deprioritize bugs with tags that overlap with in-progress items (from step 3).
4. **Actionability** — prefer bugs with clear reproduction steps, specific error messages, or identified root causes. Deprioritize bugs described vaguely or tagged with unrelated/unknown subsystems.
5. **Scope** — prefer smaller, well-scoped bugs over large uncertain ones. A focused codegen fix beats an open-ended design question.
6. **Recency** — among otherwise equal candidates, prefer older bugs (longer outstanding).

### 5. Read top candidates

- For the top 3–5 candidates, call `mcp__tracker__get` to read full details (description, notes, linked context).
- Skim the relevant source area briefly (e.g., grep for the error message or read the referenced file) to confirm the bug is still present and the fix seems feasible.
- If a bug turns out to be already fixed, stale, or not reproducible, update it via `mcp__tracker__update` (status: `cant_reproduce` or `done` with a note) and move to the next candidate.

### 6. Select and explain

- Pick the single best bug to fix.
- State your selection concisely: the bug ID, title, a one-sentence rationale for why this is the optimal next fix (e.g., "B0042 is high priority, has a clear repro, and unblocks T0015").

### 7. Chain to /do

- Invoke `/do <bug-id>` to implement the fix.

## Selection examples

| Scenario | Outcome |
|----------|---------|
| One critical bug, several medium | Pick the critical bug |
| Two high-priority bugs, one blocks a task | Pick the blocker |
| All medium priority, one has a clear root cause in notes | Pick the one with the clear root cause |
| User passes `codegen scheduler` | Only consider bugs tagged `codegen` or `scheduler` |
| Bug tagged `windows` but host is `darwin` | Skip it |
| Bug tagged `codegen`, another `codegen` item is in-progress | Deprioritize to reduce conflict risk |
| No open bugs remain | Report "No open bugs to fix" and stop |
