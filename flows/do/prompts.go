package main

import (
	"fmt"
	"strings"

	flowsdk "djabi.dev/go/flow_sdk"
)

// flowName is this flow binary's name. It must match the build target
// (./bin/flow/do) and the item.Flow value the tracker dispatches against.
const flowName flowsdk.FlowName = "do"

// defaultAgent is the attribution identity recorded on the flow's domain writes
// (notes, artifacts) when the context file carries no explicit agent.
const defaultAgent flowsdk.AgentName = "flow-do"

// prompts.go builds the per-step instructions the flow drives the agent with.
// They carry the ESSENTIAL content of the Promise project's .claude/skills/{plan,
// implement,review,coverage,inspect}; the lifecycle/harness mechanics those skills
// also carry (capture-patch, mark-done, ask-tool selection, "run verify in the
// foreground") are intentionally omitted because the FLOW owns them — this keeps
// the flow canonical while the skills stay the hand-run path. Keep the two in
// sync: when a skill's substantive guidance changes, update the matching prompt
// here.
//
// These prompts are Promise-specific (the rest of the flow is project-agnostic):
// they reference the compiler pipeline (parser → sema → ownership → codegen), the
// zero-leak policy, batch tests, and Promise's build/verify/test commands
// (`bin/build`, `bin/verify --wasm`, `bin/promise test`).
//
// Division of labor: the AGENT does the thinking/coding for the step; the FLOW
// records the durable artifact from the turn output (it is the artifact writer).
// The agent may use the tracker MCP for genuine domain decisions — asking the
// user (mcp__tracker__ask_user_question) or closing an infeasible/blocked item —
// which the flow detects via a post-turn item re-fetch.

// itemHeader is the common preamble identifying the item under work.
func itemHeader(it *flowsdk.Item) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are working on tracker item %s (type=%s).\n", it.ID, it.Type)
	fmt.Fprintf(&b, "Title: %s\n", it.Title)
	if it.Description != "" {
		fmt.Fprintf(&b, "\nDescription:\n%s\n", it.Description)
	}
	return b.String()
}

const askGuidance = "\nIf you need a decision or clarification from the user, call the " +
	"mcp__tracker__ask_user_question MCP tool (never ask in plain text) and stop.\n"

func planPrompt(it *flowsdk.Item) string {
	return itemHeader(it) + `
Produce a concrete implementation plan for this item.

- Read the item's notes and attachments and the relevant source to understand
  current behavior. For compiler changes, trace the pipeline stage
  (parser → sema → ownership → codegen). For Promise/stdlib changes, read the
  module source (modules/) and its tests. If this is a bug, reproduce it first
  (a minimal failing test or command).
- Identify which files change and what the change is; prefer the simplest approach.
  Follow the implementation philosophy: prefer Promise over hand-written IR, full
  English words in APIs, getters for side-effect-free parameterless access, ` + "`" + `doc` + "`" + `
  annotations on ` + "`" + `public` + "`" + ` declarations.
- Evaluate feasibility; for the non-feasible cases use the tracker MCP, then stop:
  - BLOCKED by unfinished work → identify (or create) the blocking item, set
    blocked_by on this item, and add the "needs-attention" tag.
  - NOT FEASIBLE as specified → set status wontfix with a clear summary, and add
    the "needs-attention" tag.
- Otherwise, output the implementation plan as your final message in Markdown.
` + askGuidance
}

func phasesPrompt(it *flowsdk.Item) string {
	return itemHeader(it) + `
Break this PLAN into discrete, independently-executable phase tasks and file them.

- Read the plan above (and the relevant code) to decide the phases.
- For each phase, create a task with the tracker MCP: mcp__tracker__create with
  type "task", a clear title + description, and parent_id set to THIS plan's id.
- Keep phases minimally coupled and ordered; only split where it genuinely helps.
- Then set THIS plan's phase list to the created task IDs: call mcp__tracker__update
  with phases set to the comma-separated child task IDs (in order).
- Do NOT implement the phases — each filed task gets its own lifecycle later.
` + askGuidance
}

func implementPrompt(it *flowsdk.Item) string {
	// The plan is a guaranteed prerequisite (stepImplementation fails fast if it is
	// missing), so the prompt always carries a real plan.
	return itemHeader(it) + `
Implement the plan below. Keep changes minimal and focused — don't refactor or add
unrelated features.

Plan:
` + strings.TrimSpace(string(it.Plan)) + `

- Make the code changes. For compiler changes, update both the implementation and
  any affected LLVM IR patterns. For Promise/stdlib changes, run ` + "`bin/build`" + ` to
  re-embed updated modules (NEVER run ` + "`go build`" + ` directly — it produces a broken
  binary).
- NEVER work around a compiler/language bug or test-infra issue — if you hit a
  limitation, file it with mcp__tracker__create and stop.
- Watch for silent systemic bugs in code you touch: MEMORY LEAKS (zero tolerance —
  the repo has 0 leaks and 0 ` + "`allow_leaks`" + ` tags; every droppable/heap-allocating
  type needs a drop path), missing scope cleanup, concurrency races (lock ordering,
  park/wake, channel close), and resource waste. File any at critical priority.
- Write tests for every behavioral change. Go: ` + "`generateIR`" + `+` + "`assertContains`" + ` (codegen),
  ` + "`checkErrs`" + `+` + "`expectError`" + ` (sema), ` + "`ownerOK`" + `/` + "`ownerErrs`" + ` (ownership). Promise:
  batch tests (` + "`test`" + ` annotation + ` + "`assert()`" + `) co-located as ` + "`*_test.pr`" + `, unless
  exact output matters.
- Run ` + "`bin/verify --wasm`" + ` and ensure it passes (it can take many minutes — wait for
  it). The output must show 0 leaks; any leak is a regression you must fix. NEVER add
  ` + "`allow_leaks: true`" + `.
- Do NOT commit or push — a later step handles that.
- End with a concise resolution summary of what you changed and why — it is recorded
  as the item's Resolution Summary for the user and the later inspection step.
` + askGuidance
}

// fixVerifyPrompt re-prompts the implementing agent (in the same session) when the
// implementation step has not yet left a passing `bin/verify` marker. `why` is the
// flow's reason (marker missing, or a failing/leaking metric). The agent must keep
// working until `bin/verify --wasm` passes cleanly — the step is not complete until
// then.
func fixVerifyPrompt(it *flowsdk.Item, why string) string {
	return fmt.Sprintf(`The implementation for %s is NOT complete: %s

Continue working on the implementation. The step is complete only when `+"`bin/verify --wasm`"+`
passes cleanly on the current changes — 0 test failures and 0 leaks (the repo's
zero-leak policy is absolute; never add `+"`allow_leaks: true`"+`).

- Diagnose and fix the failing tests / leaks (or the missing build), making real code
  changes — do not skip, disable, or weaken tests to get past verify.
- If you changed Promise/stdlib modules, run `+"`bin/build`"+` to re-embed them first.
- Then run `+"`bin/verify --wasm`"+` in the foreground and wait for it to finish; repeat
  until it is fully green.
- Do NOT commit or push — a later step handles that.
- If the work is genuinely infeasible or you need a user decision, use the tracker MCP
  (close as not-feasible, or mcp__tracker__ask_user_question) and stop.
`, it.ID, why)
}

// rebaseConflictPrompt drives the smart conflict-resolution turn of the commit
// step. The flow already committed the work and rebased onto the latest origin, and
// the rebase stopped on merge conflicts (the runner cannot resolve them). It mirrors
// the commit skill's conflict guidance ("resolve them carefully and continue the
// rebase", then re-verify with the zero-leak check). `rb` carries the rebase output.
func rebaseConflictPrompt(it *flowsdk.Item, rb *flowsdk.ArenaResult) string {
	return itemHeader(it) + `
Your committed change for this item was rebased onto the latest origin, and the
rebase STOPPED ON MERGE CONFLICTS. Resolve them so the rebase completes on top of the
remote.

Rebase output:
` + truncate(strings.TrimSpace(rb.Output+"\n"+rb.Error), 800) + `

- Run ` + "`git status`" + ` to see the in-progress rebase and the conflicted files.
- Resolve each conflict carefully: read the whole file, integrate BOTH sides (keep the
  intent of your change AND the incoming changes from origin). Do NOT blindly pick one
  side or delete code to make the markers (` + "`<<<<<<<`/`=======`/`>>>>>>>`" + `) go away.
- ` + "`git add`" + ` each resolved file, then ` + "`git rebase --continue`" + `. Repeat until the
  rebase is fully applied (it may stop on several commits).
- Then run ` + "`bin/verify --wasm`" + ` and make it pass on the merged result — a rebase can
  introduce SEMANTIC conflicts git did not flag. It must show 0 test failures and 0
  leaks (zero-leak policy is absolute; never add ` + "`allow_leaks: true`" + `). If you changed
  Promise/stdlib modules, run ` + "`bin/build`" + ` first to re-embed them.
- Leave the worktree clean and the rebase complete. Do NOT ` + "`git push`" + ` (a later step
  pushes) and do NOT ` + "`git rebase --abort`" + ` (that throws away the rebase progress).
` + askGuidance
}

// fixRebaseConflictPrompt re-prompts the resolving agent (same session) when the
// rebase is not yet finished cleanly: still in progress / unresolved conflicts, or
// verify is not green on the merged result. `why` is the flow's reason.
func fixRebaseConflictPrompt(it *flowsdk.Item, why string) string {
	return fmt.Sprintf(`The rebase for %s is NOT finished cleanly yet: %s

Keep going. The step is complete only when the rebase is fully applied (no rebase in
progress, no remaining conflict markers) and `+"`bin/verify --wasm`"+` passes on the merged
result — 0 failures and 0 leaks (never add `+"`allow_leaks: true`"+`).

- Run `+"`git status`"+`; if a rebase is still in progress, resolve the remaining conflicts,
  `+"`git add`"+` them, and `+"`git rebase --continue`"+` until it completes.
- Then run `+"`bin/verify --wasm`"+` in the foreground and wait for it; fix any failures or
  leaks the merge introduced (run `+"`bin/build`"+` first if you changed modules).
- Do NOT `+"`git push`"+` or `+"`git rebase --abort`"+`.
`, it.ID, why)
}

func reviewPrompt(it *flowsdk.Item) string {
	if it.Type == flowsdk.ItemPlan {
		return itemHeader(it) + `
Critically review THIS PLAN before it is broken into phase tasks.

- Read the plan above (and the relevant code) and check it for completeness,
  feasibility, correct decomposition, sensible ordering, and any missing work.
- Improve the plan directly — update it via the tracker MCP; don't just list concerns.
- End with a concise review summary (what you checked, what you changed).
` + askGuidance
	}
	return itemHeader(it) + `
Review your recent changes for this item (start from ` + "`git diff`" + ` / ` + "`git diff --cached`" + `).

- Read every changed file in full (not just the diff hunks) to understand context.
- Check correctness: logic errors, missed edge cases, error handling (failable ` + "`!`" + `
  results must be propagated, not silently dropped), ownership (moves ` + "`~`" + `, borrows
  ` + "`&`" + `, drop flags cleared at move sites, scope-cleanup ordering), and concurrency
  (data races on shared maps, lock ordering, park/wake). For codegen, verify the LLVM
  IR shape (correct types, no dangling refs, null checks). For sema, check all 4 passes
  (Declare, Define, Check, Verify) are consistent. For scheduler code, check the park
  mutex protocol, address-ordered lock discipline, and shutdown.
- MEMORY (zero tolerance): every heap-allocating type must have a drop path; any leak
  in verify output is a regression. Shared mutable state must be mutex-protected. Flag
  ` + "`allow_leaks: true`" + ` anywhere as a blocking issue.
- Check conventions (full English API names, getters, ` + "`doc`" + ` annotations) and that
  every behavioral change has a test.
- Fix issues directly; for anything you can't fix (compiler limitation, design
  question), file a tracker item (mcp__tracker__create).
- After fixing, confirm it still builds and tests pass (` + "`bin/verify --wasm`" + `).
- End with a concise review summary (what you reviewed, what you fixed, what you filed).
` + askGuidance
}

func coveragePrompt(it *flowsdk.Item) string {
	return itemHeader(it) + `
Analyze and fill test-coverage gaps for the changes on this item.

- For Go code: from ` + "`compiler/`" + `, run
  ` + "`go test ./<package>/ -coverprofile=/tmp/cov.out -count=1`" + ` then
  ` + "`go tool cover -func=/tmp/cov.out`" + ` to find low-coverage functions.
- For Promise code: run ` + "`bin/promise test -coverage <file-or-dir>`" + ` (e.g.
  ` + "`bin/promise test -coverage tests/e2e/...`" + `) to get block-level coverage.
- Add missing tests (error paths, edge cases, state transitions) following existing
  patterns, and run them to make sure they pass. Prefer batch tests for Promise.
- Audit resource invariants, not just line coverage: every heap-allocating type
  should have a test that confirms cleanup (no leak); concurrency code should have a
  stress test. File a critical bug for any leak source with no drop path.
- For genuinely untestable code (needs claude / network / external processes, or a
  missing language feature), file a tracker item (mcp__tracker__create) instead.
- End your message with a single line stating the resulting coverage, exactly:
  COVERAGE: adequate     (or)  COVERAGE: insufficient     (or)  COVERAGE: none
` + askGuidance
}

func inspectPrompt(it *flowsdk.Item) string {
	return itemHeader(it) + `
Independently inspect the completed work for this item. The implementing agent
already self-reviewed; verify their claims against the actual code. Read the item's
summary (their self-report), notes, and chat_log, then read the diff and the changed
files IN FULL and compare those claims to reality.

Do NOT change code, and do NOT run the build or tests — this is a read-only inspection
(verify already ran before this step). Focus on substance over style, and be specific
(cite file and line). For compiler changes check LLVM IR shape, sema 4-pass
consistency, ownership, and scheduler invariants; for Promise/stdlib check
conventions and the zero-leak policy.

Assess completion (does it meet the description?), quality, and test coverage.
End your message with a fenced JSON block of this exact shape:

` + "```json" + `
{
  "verdict": "pass | concerns | fail",
  "quality": "good | acceptable | poor",
  "completeness": "full | partial | minimal | none",
  "summary": "Markdown findings.",
  "tags": ["area-tag"],
  "suggestions": [
    {"title": "...", "type": "task|bug", "description": "...", "priority": "low|medium|high|critical", "rationale": "the concern this addresses", "key": "stable-dedup-key"}
  ]
}
` + "```" + `
Include suggestions only for genuine follow-ups; use an empty array otherwise.
`
}
