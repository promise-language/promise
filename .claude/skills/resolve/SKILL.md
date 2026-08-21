---
name: resolve
description: Resolve a tracker item end-to-end by driving `bin/do resolve T#` to completion. Monitors the flow, auto-unblocks routine (budget) parks with `do grant`, and stops to ask for approval when resource consumption grows unreasonable or a park needs a real decision.
---

Drive a tracker item through its `do-task` lifecycle to a finalized state using the flow binary (`bin/do`), watching progress and intervening only when needed.

## Inputs

- `$ARGUMENTS` — the tracker item id to resolve (e.g. `T1640`), optionally followed by tuning flags:
  - `--max-cost <USD>` — cumulative cost ceiling across all steps before pausing for approval. Default: **10**.
  - `--max-grants <N>` — number of automatic budget top-ups allowed before pausing for approval. Default: **5**.
  - `--max-invocations <N>` — cumulative invocation ceiling across all steps before pausing. Default: **40**.

  If no item id is given, run `bin/do status` (no arg) to see the active claim; if there is none, ask the user which item to resolve. Never guess an id.

## What `bin/do resolve` does

`bin/do resolve T#` claims the item and runs **all** lifecycle steps (plan → implementation → review → …) until the item is **finalized** or it **parks**. A park means a step hit a wall and needs an operator: usually a budget axis ran out (invocations / cost_usd / prompts_per_invocation / timeout_seconds), occasionally something that needs a human decision. Each step self-limits against its budget, so a runaway step parks rather than burning resources unbounded — the real cost risk is *repeated* automatic top-ups, which this skill governs.

## Steps

### 1. Establish a baseline

- Run `bin/do status T# --json` and record the starting cumulative `cost_usd.used` and `invocations.used` summed across all steps. Note `finalized` and `park`.
- Initialize counters: `grants_used = 0`.

### 2. Drive the flow

- Run `bin/do resolve T#` **in the background** (`run_in_background: true`) so you can observe it. It returns when the item finalizes or parks.
- While it runs, poll `bin/do status T# --json` periodically (every ~30–60s). On each poll, sum `cost_usd.used` and `invocations.used` across steps. **If either crosses its ceiling mid-run** (`--max-cost` / `--max-invocations`), go to **Step 5 (Ask for approval)** — do not wait for a park.
- When `bin/do resolve` exits, read `bin/do status T# --json` and branch on state.

### 3. Finalized → done

- If `finalized` is `true`, the item is resolved. Report a concise summary: item id + title, steps completed, total `cost_usd.used` and `invocations.used` consumed, and how many grants you issued. Stop.

### 4. Parked → classify and react

Read the `park` object from status. Classify the park by asking `bin/do grant` itself — it is the authoritative classifier:

- Run `bin/do grant --dry-run`.
  - **`grant` would top up (a budget park — the "silly reason" case):** this is a routine stall, unblock it *if within limits*:
    - Compute current cumulative `cost_usd.used` and `invocations.used`. If they are **at or over** any ceiling, or `grants_used >= --max-grants`, go to **Step 5**.
    - Otherwise run `bin/do grant` (no args — it tops up exactly the parked axis, plus any other axis already at its cap), increment `grants_used`, and go back to **Step 2** to re-run `bin/do resolve T#`.
  - **`grant` refuses** (parked on something that is not a budget axis — needs a real decision, an answer, a blocked dependency, a repeated failure, or an ambiguous plan): do **not** force it. Go to **Step 5** and surface the park reason verbatim for the user to decide. Granting budget would not unpark it.

A park is "silly" only when it is a plain budget top-up that clearly should just continue. Anything that asks a question, reports a failure, or is blocked is a real decision — escalate it, never paper over it.

### 5. Ask for approval (resource ceiling hit or non-budget park)

Use `AskUserQuestion` to pause and get a decision. Include the concrete numbers so the user can judge:
- Item id + title, current step, cumulative `cost_usd.used` and `invocations.used`, `grants_used`, and the park reason (verbatim from `park`).
- For a **resource ceiling**, offer: continue with a raised ceiling (e.g. +\$10 / +N grants), continue once (single more grant + resolve), or stop and leave it parked for later.
- For a **non-budget park**, present the decision the flow is actually asking for; do not offer a blind "grant" — grant will not help. If it is a compiler/language/test-infra blocker, follow the repo rule: it likely belongs in the tracker as its own bug, not worked around.

Only after explicit approval, apply the chosen action (grant with the approved amount, or a specific `bin/do grant <step-id> --cost/--invocations …`) and resume at **Step 2**. If the user says stop, run `bin/do status T#` once more for the record and leave the claim as-is (the item stays parked, resumable later).

## Guardrails

- **Never** work around a flow park by editing project code, faking an artifact, or force-releasing the claim to dodge a blocker. Parks that are not budget stalls are decisions for the user.
- **Never** raise a ceiling on your own past the defaults — that is exactly the "consumes more than reasonable resources" case the user asked to be gated behind approval.
- Do not `bin/do release` unless the user asks — releasing drops the claim and abandons progress. A parked item is fine to leave parked.
- Report honestly: if the item ends parked (not finalized), say so plainly with the reason and the resources spent — do not describe a parked item as resolved.
