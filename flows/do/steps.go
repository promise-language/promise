package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	flowsdk "djabi.dev/go/flow_sdk"
)

// verifyCmd is Promise's pre-commit / pre-push gate: format + vet + the full
// host+WASM test suite (CLAUDE.md: "Always run `bin/verify --wasm` before
// committing"). The runner runs it in the worktree; ./make has already built the
// `bin/verify` tool before the flow was spawned. This is the agent-proof line of
// defense — nothing that fails verify reaches a commit or origin.
const verifyCmd = "bin/verify --wasm"

// verifyTimeout caps the verify gate. The full host+WASM suite is thousands of
// tests and legitimately runs for many minutes; this is a generous ceiling so a
// genuine hang is eventually killed without truncating a healthy run.
const verifyTimeout = "45m"

// step dispatches to the executor for the derived next step. Each executor runs
// the one step and returns the InvocationResult to Emit.
func (f *flow) step(key flowsdk.ArtifactKey) flowsdk.InvocationResult {
	switch key {
	case flowsdk.ArtifactPlan:
		return f.stepPlan()
	case flowsdk.ArtifactImplementation:
		return f.stepImplementation()
	case flowsdk.ArtifactReview:
		return f.stepReview()
	case flowsdk.ArtifactCoverage:
		return f.stepCoverage()
	case flowsdk.ArtifactCommit:
		return f.stepCommit()
	case flowsdk.ArtifactPush:
		return f.stepPush()
	case flowsdk.ArtifactInspection:
		return f.stepInspection()
	case flowsdk.ArtifactPhases:
		return f.stepPhases()
	default:
		return f.fail(key, "unknown step")
	}
}

// stepPhases (PLAN items only) drives the breakdown turn: the agent files each
// phase as a child task (parent_id = this plan) and records the plan's phase
// list. The phases artifact is that list of child task IDs. The flow can't
// enumerate the agent's created tasks, so the agent sets the plan's phases via
// the MCP; the flow re-fetches and verifies the artifact landed.
func (f *flow) stepPhases() flowsdk.InvocationResult {
	prompt := phasesPrompt(f.item)
	_, res, done := f.agentTurn(flowsdk.ArtifactPhases, prompt)
	if done {
		return res
	}
	if t := f.refresh(flowsdk.ArtifactPhases); t != nil {
		return *t
	}
	if !f.item.ArtifactPresent(flowsdk.ArtifactPhases) {
		return f.fail(flowsdk.ArtifactPhases, "no phases were filed — create the phase tasks and set the plan's phases to their IDs")
	}
	return f.ok(flowsdk.ArtifactPhases)
}

// stepPlan drives the planning turn and records the plan artifact. If the agent
// closed the item (infeasible/blocked) or asked a question, the flow exits and
// the tracker handles it.
func (f *flow) stepPlan() flowsdk.InvocationResult {
	prompt := planPrompt(f.item)
	resp, res, done := f.agentTurn(flowsdk.ArtifactPlan, prompt)
	if done {
		return res
	}
	if t := f.refresh(flowsdk.ArtifactPlan); t != nil {
		return *t
	}
	// The flow is the artifact writer; record the plan if the agent didn't set it
	// itself via the MCP (idempotent).
	if f.item.Plan == "" {
		if _, err := f.tr.SetPlan(f.ctx, f.id, resp.LastText); err != nil {
			return f.fail(flowsdk.ArtifactPlan, "set plan: "+err.Error())
		}
	}
	return f.ok(flowsdk.ArtifactPlan)
}

// maxImplementVerifyAttempts bounds how many agent turns the implement step spends
// getting `bin/verify` to pass before parking: the initial implementation turn plus
// up to (max-1) reprompts to fix what verify reports.
const maxImplementVerifyAttempts = 3

// stepImplementation drives the implementation turn(s) and produces the
// implementation artifact ONLY after `bin/verify` has passed on the worktree.
//
// The step is not complete until verify is green: the implementing agent is
// prompted to run `bin/verify --wasm` and make it pass, and the flow confirms it
// did via verify's success marker (.promise-home/gate-values.json — written by
// verify only on a fully-green run, deleted by bin/build/./make on any tree
// change, so its presence means verify passed on the current tree). If the marker
// is missing or shows failures, the flow reprompts the agent to keep working, up
// to maxImplementVerifyAttempts, then parks the item. The patch is captured
// (which makes the implementation artifact present → the step "done") ONLY after
// verify passes — never before.
func (f *flow) stepImplementation() flowsdk.InvocationResult {
	if strings.TrimSpace(string(f.item.Plan)) == "" {
		// The plan is a required prerequisite: derivation runs the plan step before
		// implementation, so reaching here with no plan is a broken precondition —
		// fail rather than implement blindly from the description.
		return f.fail(flowsdk.ArtifactImplementation,
			"no plan recorded — the plan is a required prerequisite for implementation")
	}

	// Clear any pre-existing verify marker so the post-turn check only accepts a run
	// the agent performs DURING this step. Without this, a stale marker left by a
	// previous item in this (reused) worktree — `.promise-home/` is not cleaned
	// between items, and ./make invalidates it only when tools rebuild — would let
	// the step complete with no fresh verification, defeating the gate.
	f.clearVerifyMark()

	prompt := implementPrompt(f.item)
	resp, res, done := f.agentTurn(flowsdk.ArtifactImplementation, prompt)
	if done {
		return res
	}
	if t := f.refresh(flowsdk.ArtifactImplementation); t != nil {
		return *t
	}
	// The implementing agent ends its first turn with a summary of what it changed;
	// keep it to record as the item's Resolution Summary once the step completes.
	implSummary := resp.LastText
	session := resp.SessionID

	// The step completes only once verify is green. Re-prompt (continuing the same
	// session, so the agent sees what it just did) until the verify marker passes
	// or the attempt budget is exhausted.
	for attempt := 1; ; attempt++ {
		ok, why := f.verifyPassed()
		if ok {
			break
		}
		if attempt >= maxImplementVerifyAttempts {
			return f.parkImplementVerify(why)
		}
		resp, res, done = f.agentTurnResume(flowsdk.ArtifactImplementation, fixVerifyPrompt(f.item, why), session)
		if done {
			return res
		}
		if t := f.refresh(flowsdk.ArtifactImplementation); t != nil {
			return *t
		}
		if resp.SessionID != "" {
			session = resp.SessionID
		}
	}

	// Record the agent's self-report as the item's Resolution Summary (informative,
	// shown in the UI; useful to the user and the inspection step's self-report read).
	// Best-effort and only when the agent didn't already set a (richer) summary itself
	// via the MCP — never fail the step over this non-critical field.
	if f.item.Summary == "" && strings.TrimSpace(string(implSummary)) != "" {
		f.tr.SetSummary(f.ctx, f.id, implSummary)
	}

	// Verify is green — the implementation is complete. Mark the item `done` (the
	// success target) and capture the patch (the implementation artifact). `done`
	// does NOT stop the flow: the tracker re-admits a done item while finalization
	// artifacts (review/coverage/commit/push/inspection) are still pending, so the
	// remaining steps run; the item is only truly finalized once every required
	// artifact is present. Setting `done` is also what lets the item EVER finalize —
	// a still-`open` item stays eligible forever (no step would otherwise close it).
	//
	// Order matters: set `done` BEFORE capturing the patch. The patch is the
	// artifact that advances derivation past this step, so if capture fails after
	// `done` is set the next dispatch re-runs implementation (idempotent), whereas
	// capturing first then failing to set `done` would skip ahead with the status
	// never closed.
	if _, err := f.tr.SetStatus(f.ctx, f.id, flowsdk.StatusDone); err != nil {
		return f.fail(flowsdk.ArtifactImplementation, "set status done: "+err.Error())
	}
	if _, err := f.rn.CapturePatch(f.ctx, flowsdk.CapturePatchRequest{Step: string(flowsdk.ArtifactImplementation)}); err != nil {
		return f.fail(flowsdk.ArtifactImplementation, "capture-patch: "+err.Error())
	}
	// A (re-)implementation supersedes any previously-produced downstream output.
	f.markPresentStale(flowsdk.ArtifactReview, flowsdk.ArtifactCoverage,
		flowsdk.ArtifactCommit, flowsdk.ArtifactPush, flowsdk.ArtifactInspection)
	return f.ok(flowsdk.ArtifactImplementation)
}

// gateValuesRelPath is where `bin/verify` writes its success marker (the gate-values
// sidecar). verify writes it ONLY on a fully-green run; bin/build / ./make delete it
// on any tree change — so its presence means "verify passed on the current tree".
const gateValuesRelPath = ".promise-home/gate-values.json"

// gateValues mirrors the subset of the verify marker the flow inspects.
type gateValues struct {
	Values map[string]float64 `json:"values"`
}

// clearVerifyMark removes any existing bin/verify marker from the worktree, so a
// subsequent verifyPassed() only succeeds on a run produced afterwards. Used at the
// start of the implement step to defeat a stale marker (e.g. from a prior item in a
// reused worktree). A missing marker is not an error.
func (f *flow) clearVerifyMark() {
	if f.worktree == "" {
		return
	}
	_ = os.Remove(filepath.Join(f.worktree, gateValuesRelPath))
}

// verifyPassed reports whether `bin/verify` has left a passing marker in the
// worktree, with a human reason when it has not. Presence of the marker already
// implies a green run (verify writes it only on success); the failure/leak metrics
// are re-checked defensively.
func (f *flow) verifyPassed() (bool, string) {
	if f.worktree == "" {
		return false, "cannot locate the bin/verify marker (no worktree path)"
	}
	data, err := os.ReadFile(filepath.Join(f.worktree, gateValuesRelPath))
	if err != nil {
		return false, "no successful bin/verify run found (" + gateValuesRelPath +
			" missing) — run `bin/verify --wasm` and make it pass"
	}
	var gv gateValues
	if err := json.Unmarshal(data, &gv); err != nil {
		return false, "bin/verify marker is unreadable: " + err.Error()
	}
	for k, v := range gv.Values {
		if v > 0 && (strings.HasSuffix(k, "_test_failures") || strings.HasSuffix(k, "_leak_count")) {
			return false, fmt.Sprintf("bin/verify reported %s=%.0f", k, v)
		}
	}
	return true, ""
}

// parkImplementVerify parks the item after the implement step has exhausted its
// attempts to get `bin/verify` green: a diagnosed, non-transient work failure that
// needs resolution. The lease is kept so the partial implementation is preserved.
// The patch is NOT captured (the implementation artifact stays absent), so the step
// is not "done". Returns a failed result to Emit.
func (f *flow) parkImplementVerify(why string) flowsdk.InvocationResult {
	reason := fmt.Sprintf("implementation incomplete: bin/verify did not pass after %d attempts — %s",
		maxImplementVerifyAttempts, why)
	if _, err := f.tr.Park(f.ctx, f.id, f.agent, flowsdk.ParkRequest{
		Kind:         flowsdk.FlowFailureStep,
		Transient:    false,
		ReleaseLease: false,
		Reason:       flowsdk.Markdown(reason),
	}); err != nil {
		return f.fail(flowsdk.ArtifactImplementation, reason+" (park request failed: "+err.Error()+")")
	}
	return f.fail(flowsdk.ArtifactImplementation, reason)
}

// stepReview drives the self-review turn (the agent fixes issues directly),
// records the review summary, and re-opens downstream artifacts if the review
// changed code.
func (f *flow) stepReview() flowsdk.InvocationResult {
	before, _ := f.rn.Status(f.ctx)
	prompt := reviewPrompt(f.item)
	resp, res, done := f.agentTurn(flowsdk.ArtifactReview, prompt)
	if done {
		return res
	}
	if t := f.refresh(flowsdk.ArtifactReview); t != nil {
		return *t
	}
	if _, err := f.tr.SetReviewSummary(f.ctx, f.id, f.agent, resp.LastText); err != nil {
		return f.fail(flowsdk.ArtifactReview, "set review summary: "+err.Error())
	}
	if after, _ := f.rn.Status(f.ctx); gitChanged(before, after) {
		f.rn.CapturePatch(f.ctx, flowsdk.CapturePatchRequest{Step: string(flowsdk.ArtifactReview)})
		f.markPresentStale(flowsdk.ArtifactCoverage, flowsdk.ArtifactCommit,
			flowsdk.ArtifactPush, flowsdk.ArtifactInspection)
	}
	return f.ok(flowsdk.ArtifactReview)
}

// stepCoverage drives the coverage turn (the agent adds missing tests), parses
// the coverage rating, records it, and re-opens downstream artifacts on a code
// change.
func (f *flow) stepCoverage() flowsdk.InvocationResult {
	before, _ := f.rn.Status(f.ctx)
	prompt := coveragePrompt(f.item)
	resp, res, done := f.agentTurn(flowsdk.ArtifactCoverage, prompt)
	if done {
		return res
	}
	if t := f.refresh(flowsdk.ArtifactCoverage); t != nil {
		return *t
	}
	if _, err := f.tr.SetCoverage(f.ctx, f.id, f.agent, extractCoverage(string(resp.LastText))); err != nil {
		return f.fail(flowsdk.ArtifactCoverage, "set coverage: "+err.Error())
	}
	if after, _ := f.rn.Status(f.ctx); gitChanged(before, after) {
		f.rn.CapturePatch(f.ctx, flowsdk.CapturePatchRequest{Step: string(flowsdk.ArtifactCoverage)})
		f.markPresentStale(flowsdk.ArtifactCommit, flowsdk.ArtifactPush, flowsdk.ArtifactInspection)
	}
	return f.ok(flowsdk.ArtifactCoverage)
}

// stepCommit is the SMART rebase + commit (flow-driven, no agent turn): verify the
// working tree, commit locally, then rebase onto the latest origin so the commit
// sits on top of the remote and the next (dumb) push is a fast-forward. This is
// the explicit commit step with an observable git marker — an interrupted run
// re-runs and resumes from the worktree rather than stranding uncommitted work.
// It is also what `stepPush` reactivates (by marking the commit artifact stale)
// when a push is rejected: re-running here redoes the rebase against the advanced
// remote. A rebase conflict the flow can't resolve fails the step (the tracker
// retries, then parks via the step ceiling).
func (f *flow) stepCommit() flowsdk.InvocationResult {
	valid, err := f.rn.Validate(f.ctx, flowsdk.ValidateRequest{Cmd: verifyCmd, Timeout: verifyTimeout})
	if err != nil {
		return f.fail(flowsdk.ArtifactCommit, "validate: "+err.Error())
	}
	if !valid.Success {
		return f.fail(flowsdk.ArtifactCommit, "verify failed: "+truncate(valid.Output+valid.Error, 400))
	}
	commit, err := f.rn.Commit(f.ctx, commitMessage(f.item))
	if err != nil {
		return f.fail(flowsdk.ArtifactCommit, "commit: "+err.Error())
	}
	if !commit.Success && !isNothingToCommit(commit) {
		return f.fail(flowsdk.ArtifactCommit, "commit: "+commit.Error)
	}
	// Smart rebase: fetch + rebase onto origin (the runner refuses on a dirty tree,
	// and the work is now committed, so this is safe). Makes the next push a
	// fast-forward; a conflict fails the step for retry/park.
	if rb, err := f.rn.Rebase(f.ctx); err != nil {
		return f.fail(flowsdk.ArtifactCommit, "rebase: "+err.Error())
	} else if !rb.Success {
		return f.fail(flowsdk.ArtifactCommit, "rebase: "+truncate(rb.Output+rb.Error, 400))
	}
	st, err := f.rn.Status(f.ctx)
	if err != nil || st == nil || st.LastCommit == "" {
		return f.fail(flowsdk.ArtifactCommit, "could not read commit hash")
	}
	if _, err := f.tr.SetCommit(f.ctx, f.id, f.agent, flowsdk.GitHash(st.LastCommit)); err != nil {
		return f.fail(flowsdk.ArtifactCommit, "set commit: "+err.Error())
	}
	return f.ok(flowsdk.ArtifactCommit)
}

// stepPush is the DUMB push (flow-driven, no rebase): when ahead, re-verify the
// exact state about to be pushed (bin/verify --wasm) and then push. The pre-push
// verification is the last, agent-proof line of defense — NOTHING that fails verify
// reaches origin, and because this step runs no agent turn there is no way to
// "get smart" and route around it. The commit step verified BEFORE its rebase, so
// this verification is what actually confirms the rebased merge. "Nothing to push"
// (already up-to-date) is success. On a failed verify OR a rejected push the flow
// marks the commit artifact STALE so the tracker re-runs the SMART rebase+commit
// step and then re-runs this push; persistent failure parks via the retry ceiling
// / runaway guard.
func (f *flow) stepPush() flowsdk.InvocationResult {
	st, err := f.rn.Status(f.ctx)
	if err != nil || st == nil {
		return f.fail(flowsdk.ArtifactPush, "could not read git status")
	}
	if st.Ahead > 0 {
		valid, err := f.rn.Validate(f.ctx, flowsdk.ValidateRequest{Cmd: verifyCmd, Timeout: verifyTimeout})
		if err != nil {
			return f.pushFailedReSync("pre-push validate: " + err.Error())
		}
		if !valid.Success {
			return f.pushFailedReSync("pre-push verify failed: " + truncate(valid.Output+valid.Error, 300))
		}
		res, err := f.rn.Push(f.ctx)
		if err != nil {
			return f.pushFailedReSync("push: " + err.Error())
		}
		if !res.Success && !isUpToDate(res) {
			return f.pushFailedReSync("push rejected: " + truncate(res.Output+res.Error, 300))
		}
		if cur, e := f.rn.Status(f.ctx); e == nil && cur != nil {
			st = cur
		}
	}
	if _, err := f.tr.SetPush(f.ctx, f.id, f.agent, flowsdk.GitHash(st.LastCommit)); err != nil {
		return f.fail(flowsdk.ArtifactPush, "set push: "+err.Error())
	}
	return f.ok(flowsdk.ArtifactPush)
}

// pushFailedReSync handles a push step that could not land (a failed pre-push verify
// or a rejected push): mark the commit artifact STALE to reactivate the smart
// rebase+commit step, then fail so the tracker re-dispatches (commit re-runs →
// re-verify + rebase onto the advanced remote + re-commit → push retries).
func (f *flow) pushFailedReSync(why string) flowsdk.InvocationResult {
	f.markPresentStale(flowsdk.ArtifactCommit)
	return f.fail(flowsdk.ArtifactPush, why+" — marked commit stale; the tracker will re-run the commit step")
}

// stepInspection drives the independent inspection turn, parses the verdict (and
// any follow-up suggestions), and records them. Once present, the item finalizes.
func (f *flow) stepInspection() flowsdk.InvocationResult {
	prompt := inspectPrompt(f.item)
	resp, res, done := f.agentTurn(flowsdk.ArtifactInspection, prompt)
	if done {
		return res
	}
	if t := f.refresh(flowsdk.ArtifactInspection); t != nil {
		return *t
	}
	insp, sugs := extractInspection(string(resp.LastText), f.agent)
	if _, err := f.tr.SetInspection(f.ctx, f.id, insp); err != nil {
		return f.fail(flowsdk.ArtifactInspection, "set inspection: "+err.Error())
	}
	if len(sugs) > 0 {
		f.tr.AddSuggestions(f.ctx, f.id, f.agent, sugs) // best-effort; tracker dedups by key
	}
	return f.ok(flowsdk.ArtifactInspection)
}

// ── small helpers ────────────────────────────────────────────────────────────

// commitMessage builds the mechanical commit message for the flow-driven commit
// step (no agent turn, so it cannot be prose-authored): "<id>: <title>". The
// "why" lives in the item's plan/summary on the tracker, linked by the id.
func commitMessage(it *flowsdk.Item) string {
	return string(it.ID) + ": " + it.Title
}

// gitChanged reports whether the worktree changed between two status snapshots —
// a new commit, or a different count of modified/untracked/staged files. Used to
// decide whether a review/coverage turn actually touched code (→ re-capture +
// re-open downstream artifacts).
func gitChanged(before, after *flowsdk.GitStatus) bool {
	if before == nil || after == nil {
		return after != nil // can't compare → assume a change if we have an after
	}
	return before.LastCommit != after.LastCommit ||
		before.Modified != after.Modified ||
		before.Untracked != after.Untracked ||
		before.Staged != after.Staged
}

// isNothingToCommit reports whether a commit result means "the tree had nothing
// to commit" (an idempotent re-run), which is not a failure.
func isNothingToCommit(r *flowsdk.ArenaResult) bool {
	s := strings.ToLower(r.Output + " " + r.Error)
	return strings.Contains(s, "nothing to commit") || strings.Contains(s, "no changes")
}

// isUpToDate reports whether a push result means the branch was already current.
func isUpToDate(r *flowsdk.ArenaResult) bool {
	s := strings.ToLower(r.Output + " " + r.Error)
	return strings.Contains(s, "up to date") || strings.Contains(s, "up-to-date")
}

// truncate clips s to at most n characters, appending an ellipsis when clipped.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
