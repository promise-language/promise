package main

import (
	"context"

	flowsdk "djabi.dev/go/flow_sdk"
	"djabi.dev/go/flow_sdk/runner"
	"djabi.dev/go/flow_sdk/tracker"
)

// flow holds the per-invocation clients and identity for one `do run`. A flow
// invocation is stateless across runs: it reads durable item state, executes
// exactly one step, records that step's artifact, and exits.
type flow struct {
	ctx      context.Context
	tr       *tracker.Client
	rn       *runner.Client
	id       flowsdk.ItemID
	inv      flowsdk.InvocationID
	agent    flowsdk.AgentName
	worktree string        // arena worktree root (the flow runs here; lets it read worktree files directly)
	item     *flowsdk.Item // most-recent fetch
}

// run executes exactly one step for the leased item and returns the result to
// Emit. It seeds the finalization checklist on first run, derives the next step
// from durable state (never the ledger), does the idempotent clean-pull
// preamble, then runs that one step. Completion/re-dispatch are NOT decided here
// — the tracker derives them from durable item status.
func (f *flow) run() flowsdk.InvocationResult {
	it, err := f.tr.GetItem(f.ctx, f.id)
	if err != nil {
		return f.fail("", "get item: "+err.Error())
	}
	f.item = it

	// Generic ownership preflight (shared SDK gate): refuse to touch an item that
	// selected a DIFFERENT flow, or opted out of flow processing (FlowNone). Runs
	// FIRST — before the checklist seeding below, which is itself a tracker write —
	// so a mismatched dispatch leaves durable state untouched. The refusal is a
	// clean StepSkipped; the tracker re-dispatches the correct flow.
	if res := flowsdk.Preflight(f.item, flowName, f.inv); res != nil {
		return *res
	}

	// Seed the checklist once (flow responsibility, not the tracker's): declare
	// the artifacts this item must have to finalize. The set is type-specific — a
	// plan only plans + files phases; a task/bug runs the full code lifecycle.
	if len(it.Artifacts) == 0 {
		if seeded, e := f.tr.RequireArtifacts(f.ctx, f.id, canonicalSteps(it)); e == nil && seeded != nil {
			f.item = seeded
		}
	}

	next, _ := deriveNext(f.item)
	if next == "" {
		// Finalized or terminal — nothing for the flow to do. The tracker derives
		// completion from durable state; report a no-op step.
		return f.result("", flowsdk.StepSkipped, "")
	}

	if res := f.prepareWorktree(next); res != nil {
		return *res
	}
	return f.step(next)
}

// agentTurn runs one agent (claude) turn for a step. It returns done=true (with a
// result to Emit) only when the flow must exit immediately: a no-result failure,
// or the agent asked the user a question. A successful turn returns done=false
// and the response, for the step to record its artifact from.
func (f *flow) agentTurn(step flowsdk.ArtifactKey, prompt string) (*flowsdk.RunAgentResponse, flowsdk.InvocationResult, bool) {
	return f.runAgentTurn(step, flowsdk.RunAgentRequest{
		Prompt:         prompt,
		Model:          f.item.Model,
		Effort:         f.item.Effort,
		PermissionMode: flowsdk.PermissionBypassPermissions,
		// Force a brand-new agent session for steps that must NOT reuse a prior
		// session's context: the plan step opens the flow's work, and inspection is
		// an INDEPENDENT assessment that must not inherit the implementing agent's
		// reasoning. Other steps may resume.
		FreshSession: step == flowsdk.ArtifactPlan || step == flowsdk.ArtifactInspection,
	})
}

// agentTurnResume runs a follow-up turn that CONTINUES an existing agent session
// (so the agent keeps the prior turn's context — e.g. to fix what `bin/verify`
// reported). sessionID is the SessionID from the turn being continued.
func (f *flow) agentTurnResume(step flowsdk.ArtifactKey, prompt, sessionID string) (*flowsdk.RunAgentResponse, flowsdk.InvocationResult, bool) {
	return f.runAgentTurn(step, flowsdk.RunAgentRequest{
		Prompt:         prompt,
		Model:          f.item.Model,
		Effort:         f.item.Effort,
		PermissionMode: flowsdk.PermissionBypassPermissions,
		SessionID:      sessionID,
		Resume:         true,
	})
}

// runAgentTurn runs one agent turn from a prepared request and applies the shared
// post-turn handling: a transport error or no-result failure (done, with a failed
// result), or an agent question (done, recorded → needs_answer). A successful turn
// returns done=false and the response for the caller to act on.
func (f *flow) runAgentTurn(step flowsdk.ArtifactKey, req flowsdk.RunAgentRequest) (*flowsdk.RunAgentResponse, flowsdk.InvocationResult, bool) {
	resp, err := f.rn.RunAgent(f.ctx, req)
	if err != nil {
		return nil, f.fail(step, "run_agent: "+err.Error()), true
	}
	if resp.Question != nil {
		// Record the question if the agent didn't already do so via the MCP tool
		// (e.g. it used its built-in asker). needs_answer is derived from it, so
		// the tracker parks the item and re-dispatches once the user answers.
		if cur, e := f.tr.GetItem(f.ctx, f.id); e != nil || cur == nil || !cur.NeedsAnswer() {
			f.tr.AskQuestions(f.ctx, f.id, f.agent, []flowsdk.AgentQuestion{*resp.Question})
		}
		return resp, f.ok(step), true
	}
	if !resp.Success {
		reason := "agent produced no result"
		switch {
		case resp.Failure != nil && resp.Failure.Reason != "":
			reason = resp.Failure.Reason
		case resp.ErrorText != "":
			reason = resp.ErrorText
		case resp.RateLimit != nil:
			reason = "rate-limited"
		}
		// A failed turn feeds the tracker's execution-health axis (retry/park) —
		// not the domain status. Don't park blindly here.
		return resp, f.fail(step, reason), true
	}
	return resp, flowsdk.InvocationResult{}, false
}

// refresh re-reads the item after a turn and reports whether the agent drove it
// to a terminal domain state (closed as infeasible/blocked, or needs_answer via
// the MCP tool). When the returned result is non-nil the step returns it at once.
func (f *flow) refresh(step flowsdk.ArtifactKey) *flowsdk.InvocationResult {
	it, err := f.tr.GetItem(f.ctx, f.id)
	if err != nil || it == nil {
		return nil
	}
	f.item = it
	if terminalReason(it) != "" {
		r := f.ok(step)
		return &r
	}
	return nil
}

// prepareWorktree is the setup preamble run before a step: it makes the worktree
// fit for that step, syncing it to upstream so the agent reads and builds on
// current code. It is NOT a reported step — keeping `do run` and `do status`
// next-derivation identical (both purely artifact-based). It returns a non-nil
// result when the flow must EXIT instead of running the step.
//
// Once an implementation exists the tree is intentionally dirty (the in-progress
// code), so the preamble is skipped — it must never clobber that work.
//
// The plan step is STRICT, because it reads the code to write the plan and so
// requires a clean, current tree:
//   - dirty tree → don't plan against stale/foreign changes; park transiently and
//     release the lease so the orchestrator captures the diff, frees the arena,
//     and retries on a fresh one.
//   - behind upstream → clean-pull, and it must succeed; a failed sync is the same
//     transient, release-the-arena condition.
//   - status unreadable → fail the step so it retries in place (not an arena fault).
//
// Other (pre-implementation) steps stay best-effort: sync when cleanly possible,
// but never block on a dirty tree or a status hiccup.
func (f *flow) prepareWorktree(next flowsdk.ArtifactKey) *flowsdk.InvocationResult {
	if f.item.ArtifactPresent(flowsdk.ArtifactImplementation) {
		return nil // post-implementation: the tree is intentionally dirty
	}
	plan := next == flowsdk.ArtifactPlan
	st, err := f.rn.Status(f.ctx)
	if err != nil || st == nil {
		if plan {
			r := f.fail(flowsdk.ArtifactPlan, "plan step: cannot read worktree status to verify it is clean and current")
			return &r
		}
		return nil
	}
	if !st.Clean {
		if plan {
			return f.parkArenaSetup(flowsdk.ArtifactPlan,
				"plan step: arena worktree is dirty — releasing it to retry the plan on a clean arena")
		}
		return nil // never clobber a dirty pre-implementation tree
	}
	if st.Behind > 0 {
		if _, err := f.rn.CleanPull(f.ctx); err != nil {
			if plan {
				return f.parkArenaSetup(flowsdk.ArtifactPlan,
					"plan step: failed to sync the worktree to upstream (clean-pull): "+err.Error())
			}
			return nil // best-effort sync for non-plan steps
		}
	}
	return nil
}

// parkArenaSetup reports a transient arena-setup problem (POST /park) and returns
// the result to Emit. Transient + ReleaseLease ⇒ the orchestrator captures the
// worktree diff, frees the lease, and re-dispatches onto a fresh arena after a
// backoff. The flow does not run the step; it exits with this result.
func (f *flow) parkArenaSetup(step flowsdk.ArtifactKey, reason string) *flowsdk.InvocationResult {
	if _, err := f.tr.Park(f.ctx, f.id, f.agent, flowsdk.ParkRequest{
		Kind:         flowsdk.FlowFailureArenaSetup,
		Transient:    true,
		ReleaseLease: true,
		Reason:       flowsdk.Markdown(reason),
	}); err != nil {
		r := f.fail(step, reason+" (park request failed: "+err.Error()+")")
		return &r
	}
	r := f.fail(step, reason)
	return &r
}

// markPresentStale flags the given downstream artifacts Stale, but only those
// currently present — so a (re-)run that supersedes earlier output re-opens just
// the work that was actually done, and a first pass (nothing downstream yet) is a
// no-op. The flow OWNS staleness; stale content is kept and reused on redo.
func (f *flow) markPresentStale(keys ...flowsdk.ArtifactKey) {
	var stale []flowsdk.ArtifactKey
	for _, k := range keys {
		if f.item.ArtifactPresent(k) {
			stale = append(stale, k)
		}
	}
	if len(stale) > 0 {
		f.tr.MarkStale(f.ctx, f.id, f.agent, stale)
	}
}

// result builds an InvocationResult for the given step/status.
func (f *flow) result(step flowsdk.ArtifactKey, status flowsdk.StepResult, errStr string) flowsdk.InvocationResult {
	return flowsdk.InvocationResult{
		Flow:         flowName,
		InvocationID: f.inv,
		ItemID:       f.id,
		Step:         flowsdk.StepName(step),
		Status:       status,
		Error:        errStr,
	}
}

func (f *flow) ok(step flowsdk.ArtifactKey) flowsdk.InvocationResult {
	return f.result(step, flowsdk.StepDone, "")
}

func (f *flow) fail(step flowsdk.ArtifactKey, why string) flowsdk.InvocationResult {
	return f.result(step, flowsdk.StepFailed, why)
}
