// Step handlers for the do flow. Each handler is the slim per-step body the OSS
// orchestrator invokes via cli.RunOne: read the live item state (via the tracker
// backend's GetItem helper), drive one agent turn, translate the result into a
// typed artifact, and return.
//
// The handler returns:
//
//   - nil after calling ctx.Resolve* → step success
//   - flow.ErrTransient (wrap with %w) → infra problem, park without consuming
//     an invocation
//   - ctx.AskQuestions(...) → park on a user question
//   - any other error → step failure (counts as an invocation)
//
// Bounded loops (verify-fix in implement, conflict-resolve in commit) are
// bounded by OSS step budgets (MaxPromptsPerInvocation): cli.RunOne returns
// ErrBudgetExhausted from the metered agent when the cap is hit, and the
// orchestrator parks the step.
//
// The agent prompts and the verify gate are Promise-specific (see prompts.go);
// everything else mirrors the tracker repo's project-agnostic reference do flow.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	flowsdk "djabi.dev/go/flow_sdk"
	trackerbackend "djabi.dev/go/flow_sdk/pkg/backend/tracker"
	"github.com/promise-language/flow"
)

// verifyCmd is Promise's pre-commit / pre-push gate: format + vet + the full
// host+WASM test suite (CLAUDE.md: "Always run `bin/verify --wasm` before
// committing"). The flow runs it as the agent-proof line of defense — the
// implement/commit/push steps only proceed once it passes on the worktree.
const verifyCmd = "bin/verify --wasm"

// mustJSON marshals v to a json.RawMessage; falls back to a JSON null on error
// (the input shapes are local structs we control, so an error here would be a
// programmer mistake — not a runtime condition the caller could handle).
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// maybeParkRemoteUnreachable inspects a git/arena result for a transport-layer
// failure signature (the runner classifies these via classifyRemoteError and
// stamps RemoteUnreachable+RemoteHost on the ArenaResult). When set, raises a
// transient remote-unreachable park so the tracker's remote-down state machine
// pauses dispatch + auto-resumes on probe success — instead of burning the
// task's retry budget on what is purely an infrastructure outage. Returns a
// flow.ErrTransient when it parks, or nil to signal "no transport problem,
// continue with normal error handling". T0455.
func maybeParkRemoteUnreachable(ctx flow.StepCtx, b *trackerbackend.Backend, step string, res *flowsdk.ArenaResult) error {
	if res == nil || !res.RemoteUnreachable {
		return nil
	}
	host := res.RemoteHost
	if host == "" {
		host = "(unknown)"
	}
	reason := fmt.Sprintf("git remote %s unreachable — waiting for the tracker's remote-down probe ticker to confirm recovery", host)
	details := truncate(res.Error+"\n"+res.Output, 600)
	if perr := b.ParkRemoteUnreachable(ctx.Context(), ctx.Claim(), host, reason, details); perr != nil {
		fmt.Fprintf(os.Stderr, "do %s: park remote-unreachable: %v\n", step, perr)
	}
	return fmt.Errorf("%s: %w", reason, flow.ErrTransient)
}

// makeStepPlan drives the planning turn. Reads the item, runs the agent with the
// plan prompt, records the plan markdown as the artifact — but defers to the
// agent's own MCP write when present, so a richer MCP-written plan isn't
// overwritten by a one-line LastText summary.
//
// FreshSession=true: the plan step opens the flow's work on this item; never
// inherit prior session state.
func makeStepPlan(b *trackerbackend.Backend) func(flow.StepCtx) error {
	return func(ctx flow.StepCtx) error {
		it, err := fetchItem(ctx, b)
		if err != nil {
			return err
		}
		resp, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
			Prompt:         planPrompt(it),
			PermissionMode: string(flowsdk.PermissionAuto),
			Model:          string(it.Model),
			Effort:         string(it.Effort),
			FreshSession:   true,
		})
		if err != nil {
			return err
		}
		fresh, terr := terminalOrFreshAfterTurn(ctx, b)
		if terr != nil {
			return terr
		}
		if fresh != nil && agentEndedFlow(fresh) {
			return ctx.Skip("agent drove the item to a terminal state")
		}
		// Honor an agent MCP-write: if the live item already carries a Plan (the
		// agent set it via mcp__tracker__update), resolve with THAT value rather
		// than overwriting it with the (typically shorter) LastText summary. Falls
		// through to the agent's text only when the field is empty.
		body := strings.TrimSpace(string(resp.LastText))
		if fresh != nil && strings.TrimSpace(string(fresh.Plan)) != "" {
			body = string(fresh.Plan)
		}
		if body == "" {
			return errors.New("agent returned no plan and item has none — refusing to resolve with an empty plan")
		}
		return ctx.ResolveMarkdown(body)
	}
}

// makeStepPhases (PLAN items only) drives the breakdown turn. The agent files
// each phase as a child task via MCP and sets the plan's phase list. The flow
// re-fetches and confirms phases landed, then resolves with the agent-written
// list (the backend's ResolveArtifact for phases is idempotent when the list
// already matches).
func makeStepPhases(b *trackerbackend.Backend) func(flow.StepCtx) error {
	return func(ctx flow.StepCtx) error {
		it, err := fetchItem(ctx, b)
		if err != nil {
			return err
		}
		if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
			Prompt:         phasesPrompt(it),
			PermissionMode: string(flowsdk.PermissionAuto),
			Model:          string(it.Model),
			Effort:         string(it.Effort),
		}); err != nil {
			return err
		}
		fresh, terr := terminalOrFreshAfterTurn(ctx, b)
		if terr != nil {
			return terr
		}
		if fresh != nil && agentEndedFlow(fresh) {
			return ctx.Skip("agent drove the item to a terminal state")
		}
		if fresh == nil || !fresh.ArtifactPresent(flowsdk.ArtifactPhases) {
			return errors.New("no phases were filed — create the phase tasks and set the plan's phases to their IDs")
		}
		return ctx.ResolveJSON(mustJSON(fresh.Phases))
	}
}

// makeStepImplementation drives the implement-then-verify loop. Each failed
// verify re-prompts the agent with the verify output until either verify passes
// OR MaxPromptsPerInvocation is exhausted (the metered agent surfaces
// ErrBudgetExhausted; cli.RunOne translates it to a park). The captured patch is
// the implementation artifact's content; the final ctx.ResolvePatch tells OSS
// the handler is done. NO summary is written here — the dedicated post-push
// summary step produces it so it reflects the merged result.
func makeStepImplementation(b *trackerbackend.Backend) func(flow.StepCtx) error {
	return func(ctx flow.StepCtx) error {
		plan, ok := ctx.Markdown("plan")
		if !ok || strings.TrimSpace(plan) == "" {
			return errors.New("no plan recorded — the plan is a required prerequisite for implementation")
		}
		it, err := fetchItem(ctx, b)
		if err != nil {
			return err
		}
		// Reconciliation — if the implementation work has already been committed
		// locally (a previous flow-run committed it but failed before recording the
		// artifact), recover it without running another agent turn. Narrow trigger:
		// no uncommitted work, ahead of upstream, HEAD subject is "<itemID>: …"
		// (this flow's commit signature). A foreign commit on top falls through to
		// the normal agent turn — that is the right signal that human-driven work
		// happened.
		if !it.ArtifactPresent(flowsdk.ArtifactImplementation) {
			if st, _ := b.Status(ctx.Context()); st != nil &&
				st.Modified == 0 && st.Untracked == 0 && st.Staged == 0 &&
				!st.Conflicts && st.GitInProgress == "" && st.Ahead > 0 &&
				strings.HasPrefix(st.LastCommitSubject, string(it.ID)+":") {
				if err := reconcileCommittedAhead(ctx, b, it, st); err != nil {
					return err
				}
				return ctx.ResolvePatch(flow.PatchBody{})
			}
		}
		prompt := implementPrompt(it)
		round := 0
		for {
			round++
			ctx.Notify("implementation", fmt.Sprintf("running agent (round %d)", round))
			if _, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
				Prompt:         prompt,
				PermissionMode: string(flowsdk.PermissionAuto),
				Model:          string(it.Model),
				Effort:         string(it.Effort),
			}); err != nil {
				return err
			}
			fresh, terr := terminalOrFreshAfterTurn(ctx, b)
			if terr != nil {
				return terr
			}
			if fresh != nil {
				it = fresh
			}
			if fresh != nil && agentEndedFlow(fresh) {
				return ctx.Skip("agent drove the item to a terminal state during the implement loop")
			}
			ctx.Notify("implementation", fmt.Sprintf("running verify (round %d)", round))
			gate, err := b.Validate(ctx.Context(), verifyCmd)
			if err != nil {
				return fmt.Errorf("verify: %w: %w", err, flow.ErrTransient)
			}
			if gate.Success {
				break
			}
			// Verify failed — re-prompt the agent with the output. The metered
			// agent's MaxPromptsPerInvocation cap bounds this loop; on the Nth
			// iteration the next Run call returns ErrBudgetExhausted, the
			// orchestrator parks the step, and an operator can `do grant
			// implementation --prompts N` to extend the cap.
			//
			// Preserve `it` on a refresh error rather than passing nil into the
			// prompt — implementFixPrompt → itemHeader would crash on a nil
			// receiver.
			if refreshed, _ := b.GetItem(ctx.Context(), flowsdk.ItemID(ctx.Item().ID)); refreshed != nil {
				it = refreshed
			}
			prompt = implementFixPrompt(it, gate.Output+gate.Error)
		}
		// Verify passed. Re-check the LIVE item one more time before recording the
		// implementation: the agent may have driven the item to a terminal state
		// mid-loop, in which case skip rather than capture.
		fresh, _ := b.GetItem(ctx.Context(), flowsdk.ItemID(ctx.Item().ID))
		if fresh != nil && agentEndedFlow(fresh) {
			return ctx.Skip("agent drove the item to a terminal state at the end of the implement loop")
		}
		// NO status flip here. open→done is the PUSH step's job (done == "real and
		// merged"; see makeStepPush) — the item stays open until its work is
		// committed AND pushed. The resolution summary is likewise produced by the
		// dedicated post-push summary step (makeStepSummary). The implement step's
		// only artifact is the captured patch.
		ctx.Notify("implementation", "capturing patch")
		if _, err := b.CapturePatch(ctx.Context(), "implementation"); err != nil {
			return fmt.Errorf("capture-patch: %w", err)
		}
		// Mark downstream artifacts stale if they were present (a re-implementation
		// supersedes them).
		for _, key := range []flowsdk.ArtifactKey{
			flowsdk.ArtifactReview, flowsdk.ArtifactCoverage,
			flowsdk.ArtifactCommit, flowsdk.ArtifactPush, flowsdk.ArtifactInspection,
		} {
			if fresh != nil && fresh.ArtifactPresent(key) {
				_ = b.MarkStale(ctx.Context(), ctx.Claim(), flow.ArtifactId(key))
			}
		}
		// Final resolve — Backend.ResolveArtifact's implementation case verifies the
		// patch is present (we just captured it above) and returns success.
		return ctx.ResolvePatch(flow.PatchBody{})
	}
}

// reconcileCommittedAhead recovers an implementation whose work is already
// committed-ahead but whose artifact never landed (the commit step's git commit
// succeeded but a later side effect failed before Item.CommitHash was set, so on
// retry the worktree is clean and stepImplementation can't see anything to
// capture). On a stale runner that can't produce a non-empty patch this fails
// fast with a precise diagnostic — never silently re-runs the agent against a
// clean tree.
//
// Caller must have already verified the trigger predicate (no uncommitted work,
// Ahead>0, HEAD subject matches the item ID).
func reconcileCommittedAhead(ctx flow.StepCtx, b *trackerbackend.Backend, it *flowsdk.Item, st *flowsdk.GitStatus) error {
	fmt.Fprintf(os.Stderr,
		"do implement %s: reconciling — HEAD %s subject %q matches item ID; work already committed-ahead (Ahead=%d); skipping agent turn\n",
		it.ID, st.LastCommit, st.LastCommitSubject, st.Ahead)
	ctx.Notify("implementation", "reconciling committed-ahead work")
	if _, err := b.CapturePatch(ctx.Context(), "implementation"); err != nil {
		return fmt.Errorf("reconcile capture-patch: %w", err)
	}
	// Verify the patch actually attached. A stale runner will silently produce an
	// empty patch — fail fast with an actionable diagnostic.
	fresh, _ := b.GetItem(ctx.Context(), it.ID)
	if fresh == nil || !fresh.ArtifactPresent(flowsdk.ArtifactImplementation) {
		return fmt.Errorf(
			"reconcile: HEAD %s matches item ID and is %d ahead, but CapturePatch produced no non-empty patch — runner may need the upstream..HEAD fallback",
			st.LastCommit, st.Ahead)
	}
	// Clear stale on review & coverage — they were produced against THIS
	// implementation (the one we just rediscovered as committed ahead), so they
	// are NOT stale. This is the whole point of reconciliation: keep downstream
	// artifacts fresh and let the flow advance directly to commit.
	for _, key := range []flowsdk.ArtifactKey{flowsdk.ArtifactReview, flowsdk.ArtifactCoverage} {
		if a := fresh.Artifact(key); a != nil && a.Stale {
			_ = b.ClearStale(ctx.Context(), ctx.Claim(), flow.ArtifactId(key))
		}
	}
	_, _ = b.AddNote(ctx.Context(), it.ID, flowsdk.AgentName(ctx.Flow()),
		flowsdk.Markdown(fmt.Sprintf("reconciled implementation from committed-ahead HEAD %s", st.LastCommit)))
	return nil
}

// makeStepReview drives the self-review turn (the agent fixes issues directly).
// Re-captures patch if the worktree changed and marks downstream artifacts
// stale. Honors an agent MCP-write of ReviewSummary the same way stepPlan honors
// MCP-set Plan.
func makeStepReview(b *trackerbackend.Backend) func(flow.StepCtx) error {
	return func(ctx flow.StepCtx) error {
		it, err := fetchItem(ctx, b)
		if err != nil {
			return err
		}
		before, _ := b.Status(ctx.Context())
		resp, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
			Prompt:         reviewPrompt(it),
			PermissionMode: string(flowsdk.PermissionAuto),
			Model:          string(it.Model),
			Effort:         string(it.Effort),
		})
		if err != nil {
			return err
		}
		fresh, terr := terminalOrFreshAfterTurn(ctx, b)
		if terr != nil {
			return terr
		}
		if fresh != nil && agentEndedFlow(fresh) {
			return ctx.Skip("agent drove the item to a terminal state")
		}
		if after, _ := b.Status(ctx.Context()); gitChanged(before, after) {
			_, _ = b.CapturePatch(ctx.Context(), "review")
			for _, key := range []flowsdk.ArtifactKey{
				flowsdk.ArtifactCoverage, flowsdk.ArtifactCommit,
				flowsdk.ArtifactPush, flowsdk.ArtifactInspection,
			} {
				_ = b.MarkStale(ctx.Context(), ctx.Claim(), flow.ArtifactId(key))
			}
		}
		// Honor an agent MCP-write of ReviewSummary; fall back to the turn's
		// LastText only when the field is empty.
		body := strings.TrimSpace(string(resp.LastText))
		if fresh != nil && strings.TrimSpace(string(fresh.ReviewSummary)) != "" {
			body = string(fresh.ReviewSummary)
		}
		if body == "" {
			return errors.New("agent returned no review summary and item has none")
		}
		return ctx.ResolveMarkdown(body)
	}
}

// makeStepCoverage drives the coverage turn, parses the COVERAGE: line the agent
// emits, and records the typed coverage value.
func makeStepCoverage(b *trackerbackend.Backend) func(flow.StepCtx) error {
	return func(ctx flow.StepCtx) error {
		it, err := fetchItem(ctx, b)
		if err != nil {
			return err
		}
		before, _ := b.Status(ctx.Context())
		resp, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
			Prompt:         coveragePrompt(it),
			PermissionMode: string(flowsdk.PermissionAuto),
			Model:          string(it.Model),
			Effort:         string(it.Effort),
		})
		if err != nil {
			return err
		}
		fresh, terr := terminalOrFreshAfterTurn(ctx, b)
		if terr != nil {
			return terr
		}
		if fresh != nil && agentEndedFlow(fresh) {
			return ctx.Skip("agent drove the item to a terminal state")
		}
		if after, _ := b.Status(ctx.Context()); gitChanged(before, after) {
			_, _ = b.CapturePatch(ctx.Context(), "coverage")
			for _, key := range []flowsdk.ArtifactKey{
				flowsdk.ArtifactCommit, flowsdk.ArtifactPush, flowsdk.ArtifactInspection,
			} {
				_ = b.MarkStale(ctx.Context(), ctx.Claim(), flow.ArtifactId(key))
			}
		}
		return ctx.ResolveMarkdown(string(extractCoverage(string(resp.LastText))))
	}
}

// makeStepCommit is the DUMB local commit (T0464: the smart rebase moved to
// the push step, which holds the push lease so fetch→rebase→push is atomic at
// the tip):
//
//  1. If the worktree has uncommitted work (modified/staged != 0), verify
//     in-place then commit it.
//  2. If the worktree is clean (a previous run already committed), the
//     LastCommit is what we record.
//  3. Record the resulting commit hash and stamp the verify-marker (HEAD) so
//     the push step can skip a redundant pre-push verify.
//
// No rebase here — a behind/diverged commit is rebased onto the tip by the
// push step under the push lease, not speculatively here (which raced other
// arenas and re-verified needlessly).
func makeStepCommit(b *trackerbackend.Backend) func(flow.StepCtx) error {
	return func(ctx flow.StepCtx) error {
		it, err := fetchItem(ctx, b)
		if err != nil {
			return err
		}
		st, err := b.Status(ctx.Context())
		if err != nil || st == nil {
			return fmt.Errorf("commit: read git status: %w: %w", err, flow.ErrTransient)
		}
		hasUncommitted := st.Modified > 0 || st.Staged > 0
		if hasUncommitted {
			ctx.Notify("commit", "running verify")
			gate, err := b.Validate(ctx.Context(), verifyCmd)
			if err != nil {
				return fmt.Errorf("verify: %w: %w", err, flow.ErrTransient)
			}
			if !gate.Success {
				return fmt.Errorf("verify failed: %s", truncate(gate.Output+gate.Error, 400))
			}
			ctx.Notify("commit", "committing")
			res, err := b.Commit(ctx.Context(), buildCommitMessage(it))
			if err != nil {
				return fmt.Errorf("commit: %w: %w", err, flow.ErrTransient)
			}
			if pe := maybeParkRemoteUnreachable(ctx, b, "commit", res); pe != nil {
				return pe
			}
			if res != nil && !res.Success && !trackerbackend.IsNothingToCommit(res) {
				return fmt.Errorf("commit: %s", truncate(res.Output+res.Error, 400))
			}
			// Re-read status so VerifiedCommit is keyed on the post-commit
			// HEAD (the SHA we just verified clean).
			st, err = b.Status(ctx.Context())
			if err != nil || st == nil {
				return fmt.Errorf("commit: read git status after commit: %w: %w", err, flow.ErrTransient)
			}
			if st.LastCommit != "" {
				// Best-effort marker write. A failure is logged but does not
				// fail the step — the worst case is a redundant pre-push verify.
				if _, verr := b.SetVerifiedCommit(ctx.Context(), it.ID, flowsdk.GitHash(st.LastCommit)); verr != nil {
					fmt.Fprintf(os.Stderr, "do commit %s: set verified_commit=%s: %v\n", it.ID, st.LastCommit, verr)
				}
			}
		}
		if st == nil || st.LastCommit == "" {
			return errors.New("could not read commit hash after commit")
		}
		return ctx.ResolveCommitHash(st.LastCommit)
	}
}

// smartRebase drives the rebase loop, invoked by the push step on a
// non-fast-forward rejection (T0464: moved out of the commit step). On a clean rebase
// returns nil. On a conflict drives an agent turn with rebaseConflictPrompt, then
// calls RebaseContinue. Loops up to rebaseResolveMaxRounds(); on cap or
// non-conflict failure aborts the rebase and returns a step failure.
//
// Returns nil on success; a non-nil *error on failure that should be returned
// from the handler.
func smartRebase(ctx flow.StepCtx, b *trackerbackend.Backend, it *flowsdk.Item) *error {
	rb, err := b.RebaseSmart(ctx.Context())
	if err != nil {
		e := fmt.Errorf("rebase: %w: %w", err, flow.ErrTransient)
		return &e
	}
	if rb.Success {
		return nil
	}
	// T0455: a fetch/transport error during the rebase is infrastructure,
	// not a stale-commit conflict — park transient so the task isn't blamed
	// for a network outage (smartRebase runs in the push step's non-ff retry).
	if pe := maybeParkRemoteUnreachable(ctx, b, "commit", rb); pe != nil {
		return &pe
	}
	conflicts := trackerbackend.ParseRebaseConflicts(rb.Output)
	if len(conflicts) == 0 {
		// Non-recoverable: the runner has already auto-aborted to restore the
		// worktree.
		e := fmt.Errorf("rebase: %s", truncate(rb.Output+rb.Error, 400))
		return &e
	}
	maxRounds := rebaseResolveMaxRounds()
	for round := 1; ; round++ {
		if maxRounds > 0 && round > maxRounds {
			abortRebase(ctx, b, fmt.Sprintf("max rounds reached (%d)", maxRounds))
			e := fmt.Errorf(
				"rebase: unresolved conflicts in %s after %d round(s) — cap TRACKER_REBASE_RESOLVE_MAX_ROUNDS=%d reached; rebase aborted",
				strings.Join(conflicts, ", "), round-1, maxRounds)
			return &e
		}
		ctx.Notify("commit", fmt.Sprintf("resolving rebase conflicts (round %d)", round))
		_, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
			Prompt:         rebaseConflictPrompt(it, conflicts, rb.Output+rb.Error),
			PermissionMode: string(flowsdk.PermissionAuto),
			Model:          string(it.Model),
			Effort:         string(it.Effort),
		})
		if err != nil {
			abortRebase(ctx, b, "agent turn exited without resolution")
			e := fmt.Errorf("rebase conflict resolve: %w", err)
			return &e
		}
		cont, cerr := b.RebaseContinue(ctx.Context())
		if cerr != nil {
			abortRebase(ctx, b, "rebase-continue transport error")
			e := fmt.Errorf("rebase-continue: %w: %w", cerr, flow.ErrTransient)
			return &e
		}
		// T0455: remote-unreachable during rebase-continue is infra, not a
		// task fault — park transient instead of failing the step.
		if pe := maybeParkRemoteUnreachable(ctx, b, "commit", cont); pe != nil {
			return &pe
		}
		if cont.Success {
			return nil
		}
		next := trackerbackend.ParseRebaseConflicts(cont.Output)
		if len(next) == 0 {
			abortRebase(ctx, b, "rebase-continue non-conflict failure")
			e := fmt.Errorf("rebase-continue: %s", truncate(cont.Output+cont.Error, 400))
			return &e
		}
		conflicts = next
		rb = cont
	}
}

// abortRebase best-effort aborts an in-progress rebase. Logs to stderr; never
// returns an error — the caller is already on the failure path.
func abortRebase(ctx flow.StepCtx, b *trackerbackend.Backend, why string) {
	if _, err := b.RebaseAbort(ctx.Context()); err != nil {
		fmt.Fprintf(os.Stderr, "do commit: rebase-abort after %q failed: %v\n", why, err)
	}
}

// defaultRebaseResolveMaxRounds bounds smartRebase's resolution loop by default:
// after this many conflict-resolution rounds the rebase is aborted and the step
// fails for tracker retry/park. A bound is essential — an agent that can never
// produce a marker-free resolution would otherwise drain tokens indefinitely.
// Override with TRACKER_REBASE_RESOLVE_MAX_ROUNDS (0 = unlimited).
const defaultRebaseResolveMaxRounds = 3

// rebaseResolveMaxRounds resolves the conflict-resolution round cap from
// TRACKER_REBASE_RESOLVE_MAX_ROUNDS. Non-negative int overrides the default
// (0 = unlimited, N>0 = cap at N rounds). Invalid / negative falls back to
// defaultRebaseResolveMaxRounds. When the cap fires the step failure reason names
// the env var (no-hidden-timeouts contract).
func rebaseResolveMaxRounds() int {
	if v := strings.TrimSpace(os.Getenv("TRACKER_REBASE_RESOLVE_MAX_ROUNDS")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			return n
		}
	}
	return defaultRebaseResolveMaxRounds
}

// defaultPushRebaseMaxRounds bounds the push step's optimistic-push →
// (non-fast-forward) → fetch+rebase → retry loop. Default 1: we attempt a
// single rebase onto the tip and one retry; if the push still can't
// fast-forward, the step FAILS and the lease is released, and we rely on the
// orchestrator to re-dispatch the push step later (or park it once the step's
// retry budget is spent). We deliberately do NOT loop rebases within one
// invocation: the rebase rounds are not budget-tracked the way step retries
// and the agent prompt budget are, so a >1 cap would let a single invocation
// stack rebases (and, multiplied by the step retry budget, many smart
// attempts). Because the push runs under the exclusive (repoURL, branch)
// lease, one rebase is normally enough anyway. Override with
// TRACKER_PUSH_REBASE_MAX_ROUNDS (0 = unlimited).
const defaultPushRebaseMaxRounds = 1

// pushRebaseMaxRounds resolves the push-retry-after-rebase cap from
// TRACKER_PUSH_REBASE_MAX_ROUNDS. Non-negative int overrides the default
// (0 = unlimited, N>0 = at most N rebase rounds). Invalid / negative falls
// back to defaultPushRebaseMaxRounds. When the cap fires the step failure
// reason names the env var (no-hidden-timeouts contract).
func pushRebaseMaxRounds() int {
	if v := strings.TrimSpace(os.Getenv("TRACKER_PUSH_REBASE_MAX_ROUNDS")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			return n
		}
	}
	return defaultPushRebaseMaxRounds
}

// prePushVerify runs the project verify command against the worktree as it
// stands, UNLESS the verify-marker fast-path applies (HEAD ==
// Item.VerifiedCommit AND worktree clean), in which case the commit step
// already verified this exact tree and we skip. On verify failure it routes
// through pushReSync — the committed tree does not pass verify, so the commit
// step "did not do its job" and must re-run. On success it refreshes the
// marker so a subsequent push re-attempt can skip the (expensive) verify.
// `label` is the Notify detail so the journal can tell the initial verify from
// a post-rebase re-verify.
func prePushVerify(ctx flow.StepCtx, b *trackerbackend.Backend, it *flowsdk.Item, st *flowsdk.GitStatus, label string) error {
	if st.LastCommit != "" && st.LastCommit == string(it.VerifiedCommit) &&
		st.Modified == 0 && st.Staged == 0 {
		ctx.Notify("push", "skip "+label+" (marker matches HEAD, worktree clean)")
		return nil
	}
	ctx.Notify("push", label)
	gate, err := b.Validate(ctx.Context(), verifyCmd)
	if err != nil {
		return pushReSync(b, ctx, label+": "+err.Error())
	}
	if !gate.Success {
		return pushReSync(b, ctx, label+" failed: "+truncate(gate.Output+gate.Error, 300))
	}
	// Verify passed — refresh the marker so a subsequent push re-attempt can
	// skip verify even if VerifiedCommit was cleared earlier.
	if _, verr := b.SetVerifiedCommit(ctx.Context(), it.ID, flowsdk.GitHash(st.LastCommit)); verr != nil {
		fmt.Fprintf(os.Stderr, "do push %s: refresh verified_commit=%s: %v\n", it.ID, st.LastCommit, verr)
	}
	return nil
}

// resolvePushAndFlipStatus is the common tail of the push step: flip the item's
// status to done (unless the agent already drove it to a terminal state) and
// resolve the push artifact with the pushed hash. done == "real and merged":
// the open→done flip happens HERE, the single status-transition point of the
// flow — the work is now committed AND pushed to origin.
func resolvePushAndFlipStatus(ctx flow.StepCtx, b *trackerbackend.Backend, hash string) error {
	if fresh, _ := b.GetItem(ctx.Context(), flowsdk.ItemID(ctx.Item().ID)); fresh == nil || !agentEndedFlow(fresh) {
		if _, err := b.SetItemStatus(ctx.Context(), flowsdk.ItemID(ctx.Item().ID), flowsdk.StatusDone); err != nil {
			return fmt.Errorf("push: set status done: %w", err)
		}
	}
	return ctx.ResolveCommitHash(hash)
}

// makeStepPush pushes the committed work to origin under the durable
// tracker-side push lease keyed by (repoURL, branch) — T0464.
//
// Lock discipline: the expensive pre-push verify runs LOCK-FREE — it is
// read-only, so holding the lease across a multi-minute verify would
// needlessly block every other arena targeting this branch. The lease is
// acquired immediately BEFORE the push, so the critical section is just the
// push (+ a fetch+rebase retry if origin moved).
//
// Concurrency model: we do NOT pre-check "behind" before pushing (that needs a
// fetch and races other arenas, and a stale local origin ref made the old
// commit-step pre-rebase loop bounce push→commit until the budget parked it).
// Instead we push optimistically; only a genuine non-fast-forward rejection
// triggers fetch → rebase onto the tip → re-verify → retry. Because we hold the
// EXCLUSIVE lease, no other arena can move origin between our rebase and our
// retry, so the retry is guaranteed to fast-forward. A failed verify, or a
// rejection that is NOT a non-fast-forward (auth / hook / protected branch),
// still routes through pushReSync (re-run the commit step) — fetch+rebase
// cannot help those.
func makeStepPush(b *trackerbackend.Backend) func(flow.StepCtx) error {
	return func(ctx flow.StepCtx) error {
		st, err := b.Status(ctx.Context())
		if err != nil || st == nil {
			return fmt.Errorf("push: read git status: %w: %w", err, flow.ErrTransient)
		}
		if st.Ahead <= 0 {
			// Nothing to push — record the existing hash.
			if st.LastCommit == "" {
				return errors.New("could not read pushed hash")
			}
			return resolvePushAndFlipStatus(ctx, b, st.LastCommit)
		}

		it, err := fetchItem(ctx, b)
		if err != nil {
			return err
		}
		// Re-read the item so any HEAD-mutating side effect between commit and
		// here is reflected in the verify-marker check below.
		if fresh, _ := b.GetItem(ctx.Context(), it.ID); fresh != nil {
			it = fresh
		}

		// Pre-push verify — LOCK-FREE (see the lock-discipline note above).
		if err := prePushVerify(ctx, b, it, st, "pre-push verify"); err != nil {
			return err
		}

		// Acquire the (repoURL, branch) push lease HERE — immediately before
		// the push, so the critical section is only the push (+ a rebase
		// retry). Blocks under the tracker's long-poll until granted (or ctx
		// cancels). Idempotent on the holder so a retry is safe.
		repoURL, branch, err := b.PushTarget(ctx.Context())
		if err != nil {
			return fmt.Errorf("push: read remote: %w: %w", err, flow.ErrTransient)
		}
		itemID := string(ctx.Item().ID)
		if err := b.AcquirePushLease(ctx.Context(), repoURL, branch, itemID, ctx.Notify); err != nil {
			return fmt.Errorf("push lease: %w: %w", err, flow.ErrTransient)
		}
		// Best-effort release. Use a fresh background context so a ctx-cancel
		// mid-step does not skip the release POST. The reconciler is the safety net.
		defer func() {
			if rerr := b.ReleasePushLease(context.Background(), repoURL, branch, itemID); rerr != nil {
				fmt.Fprintf(os.Stderr, "do push %s: release push lease: %v\n", itemID, rerr)
			}
		}()

		// Optimistic push under the lease; rebase-and-retry only on a real
		// non-fast-forward rejection (origin moved before we held the lease).
		maxRounds := pushRebaseMaxRounds()
		for round := 1; ; round++ {
			ctx.Notify("push", "pushing")
			res, err := b.Push(ctx.Context())
			if err != nil {
				return pushReSync(b, ctx, "push: "+err.Error())
			}
			// T0455: remote-unreachable is infrastructure, not "stale commit
			// needs re-syncing". Transient remote-unreachable park BEFORE
			// pushReSync so the commit artifact stays valid (no MarkStale).
			if pe := maybeParkRemoteUnreachable(ctx, b, "push", res); pe != nil {
				return pe
			}
			if res == nil || res.Success || trackerbackend.IsUpToDate(res) {
				break // pushed (or already current)
			}
			// Push was rejected. A non-fast-forward means origin moved before
			// we took the lease; everything else (auth/hook/etc.) is not
			// fixable by fetch+rebase → re-run the commit step. IsNonFastForward
			// lives in the shared flow package (string-based) so both do-flows
			// classify push rejections identically.
			if !flow.IsNonFastForward(res.Output + " " + res.Error) {
				return pushReSync(b, ctx, "push rejected: "+truncate(res.Output+res.Error, 300))
			}
			if maxRounds > 0 && round > maxRounds {
				return fmt.Errorf(
					"push: still non-fast-forward after %d rebase round(s) — cap TRACKER_PUSH_REBASE_MAX_ROUNDS=%d reached (last: %s)",
					round-1, maxRounds, truncate(res.Output+res.Error, 200))
			}
			ctx.Notify("push", fmt.Sprintf("push rejected (non-fast-forward) — fetch + rebase onto tip (round %d)", round))
			// We hold the exclusive lease: fetch + rebase onto the tip,
			// re-verify the rebased tree, then loop to retry the push.
			if fresh, _ := b.GetItem(ctx.Context(), it.ID); fresh != nil {
				it = fresh
			}
			if rerr := smartRebase(ctx, b, it); rerr != nil {
				return *rerr
			}
			// Rebase moved HEAD ⇒ the verify-marker no longer applies and the
			// rebased tree is unverified. Clear the marker, re-read item +
			// status, and re-verify UNDER the lease (releasing to verify would
			// reopen the race).
			if _, verr := b.SetVerifiedCommit(ctx.Context(), it.ID, flowsdk.GitHash("")); verr != nil {
				fmt.Fprintf(os.Stderr, "do push %s: clear verified_commit after rebase: %v\n", itemID, verr)
			}
			if fresh, _ := b.GetItem(ctx.Context(), it.ID); fresh != nil {
				it = fresh
			}
			st, err = b.Status(ctx.Context())
			if err != nil || st == nil {
				return fmt.Errorf("push: read git status after rebase: %w: %w", err, flow.ErrTransient)
			}
			if err := prePushVerify(ctx, b, it, st, "re-verify after rebase"); err != nil {
				return err
			}
		}

		st, _ = b.Status(ctx.Context())
		if st == nil || st.LastCommit == "" {
			return errors.New("could not read pushed hash")
		}
		return resolvePushAndFlipStatus(ctx, b, st.LastCommit)
	}
}

// pushReSync marks the commit artifact stale (re-activating the smart
// rebase+commit step on the next dispatch) and returns a non-transient failure
// for the current invocation.
func pushReSync(b *trackerbackend.Backend, ctx flow.StepCtx, why string) error {
	_ = b.MarkStale(ctx.Context(), ctx.Claim(), flow.ArtifactId(flowsdk.ArtifactCommit))
	return fmt.Errorf("%s — marked commit stale; the tracker will re-run the commit step", why)
}

// makeStepSummary produces the resolution summary — a short, factual TLDR of WHAT
// changed and WHY, recorded as Item.Summary (the summary artifact).
//
// It runs from the SAME continuing session as the work (NO FreshSession): the
// doer already holds the full narrative of the change, so it is the cheapest,
// best-informed writer of the recap. It runs AFTER push, so the summary describes
// the merged result rather than a mid-implementation intention.
//
// Read-only and suggestion-free by contract: it must not touch the worktree and
// must not propose follow-up work. Proposing/judging is the next step's job —
// inspection runs in a FRESH, independent session precisely so it does not
// inherit this session's framing (see makeStepInspection). Keeping summary and
// inspection separate is deliberate.
func makeStepSummary(b *trackerbackend.Backend) func(flow.StepCtx) error {
	return func(ctx flow.StepCtx) error {
		it, err := fetchItem(ctx, b)
		if err != nil {
			return err
		}
		resp, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
			Prompt:         summaryPrompt(it),
			PermissionMode: string(flowsdk.PermissionAuto),
			Model:          string(it.Model),
			Effort:         string(it.Effort),
		})
		if err != nil {
			return err
		}
		fresh, terr := terminalOrFreshAfterTurn(ctx, b)
		if terr != nil {
			return terr
		}
		if fresh != nil && agentEndedFlow(fresh) {
			return ctx.Skip("agent drove the item to a terminal state")
		}
		summary := strings.TrimSpace(string(resp.LastText))
		if summary == "" {
			return errors.New("summary step produced no text")
		}
		return ctx.ResolveMarkdown(summary)
	}
}

// makeStepInspection drives the independent inspection turn, parses the fenced
// JSON verdict, and records it.
//
// FreshSession=true: inspection must be HISTORY-BLIND. Every prior step (plan →
// … → summary) shares one continuing session, so the doer carries the full
// narrative of what it built and why. Inspection is the adversarial counter-read
// — it must judge the merged result on its own merits, never inherit the doer's
// framing/rationalizations. A fresh session is what makes the verdict independent
// rather than a self-review.
func makeStepInspection(b *trackerbackend.Backend) func(flow.StepCtx) error {
	return func(ctx flow.StepCtx) error {
		it, err := fetchItem(ctx, b)
		if err != nil {
			return err
		}
		resp, err := ctx.Agent().Run(ctx.Context(), flow.AgentRequest{
			Prompt:         inspectPrompt(it),
			PermissionMode: string(flowsdk.PermissionAuto),
			Model:          string(it.Model),
			Effort:         string(it.Effort),
			FreshSession:   true,
		})
		if err != nil {
			return err
		}
		fresh, terr := terminalOrFreshAfterTurn(ctx, b)
		if terr != nil {
			return terr
		}
		if fresh != nil && agentEndedFlow(fresh) {
			return ctx.Skip("agent drove the item to a terminal state")
		}
		insp, sugs := extractInspection(string(resp.LastText), flowsdk.AgentName(ctx.Flow()))
		if len(sugs) > 0 {
			_, _ = b.AddSuggestions(ctx.Context(), flowsdk.ItemID(ctx.Item().ID), flowsdk.AgentName(ctx.Flow()), sugs)
		}
		return ctx.ResolveJSON(mustJSON(insp))
	}
}

// ─── small helpers shared across steps ───────────────────────────────────

// fetchItem fetches the live tracker item for the StepCtx's item id. Returns
// flow.ErrTransient on transport errors so a runner blip parks the step without
// consuming an invocation.
func fetchItem(ctx flow.StepCtx, b *trackerbackend.Backend) (*flowsdk.Item, error) {
	it, err := b.GetItem(ctx.Context(), flowsdk.ItemID(ctx.Item().ID))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", err, flow.ErrTransient)
	}
	return it, nil
}

// terminalOrFreshAfterTurn refreshes the item after an agent turn and returns the
// fresh item alongside an error. The fresh item is non-nil on success; the error
// wraps flow.ErrTransient on a transport failure (so the step parks instead of
// failing). Callers check agentEndedFlow(fresh) to decide whether to
// short-circuit.
func terminalOrFreshAfterTurn(ctx flow.StepCtx, b *trackerbackend.Backend) (*flowsdk.Item, error) {
	it, err := b.GetItem(ctx.Context(), flowsdk.ItemID(ctx.Item().ID))
	if err != nil {
		return nil, fmt.Errorf("post-turn refresh: %w: %w", err, flow.ErrTransient)
	}
	return it, nil
}

// agentEndedFlow reports whether the agent's turn left the item in a state where
// the remaining finalize steps have nothing to act on: the agent ABANDONED the
// work (wontfix/…), or the item is blocked or awaiting a user answer. It
// deliberately does NOT trigger on StatusDone — "done" means the work is real and
// merged and the flow must keep finalizing.
func agentEndedFlow(it *flowsdk.Item) bool {
	return it.IsAbandoned() || it.IsBlocked() || it.NeedsAnswer()
}

// gitChanged reports whether the worktree changed between two status snapshots —
// used by review/coverage to decide whether to re-capture patch + mark
// downstream artifacts stale.
func gitChanged(before, after *flowsdk.GitStatus) bool {
	if before == nil || after == nil {
		return after != nil
	}
	return before.LastCommit != after.LastCommit ||
		before.Modified != after.Modified ||
		before.Untracked != after.Untracked ||
		before.Staged != after.Staged
}

// truncate clips s to at most n characters, appending an ellipsis.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
