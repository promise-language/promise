package main

import (
	"fmt"

	flowsdk "djabi.dev/go/flow_sdk"
)

// taskSteps is the canonical step order for a task/bug: the full code lifecycle.
// Each step produces exactly one finalization artifact (the ArtifactKey).
var taskSteps = []flowsdk.ArtifactKey{
	flowsdk.ArtifactPlan,
	flowsdk.ArtifactImplementation,
	flowsdk.ArtifactReview,
	flowsdk.ArtifactCoverage,
	flowsdk.ArtifactCommit,
	flowsdk.ArtifactPush,
	flowsdk.ArtifactInspection,
}

// planSteps is the canonical step order for a PLAN: produce the breakdown, review
// it, then file its phases as child tasks. A plan does no code work (each filed
// phase is its own item with its own full lifecycle), so it runs only this subset
// — but the breakdown is reviewed before it is committed to as phase tasks.
var planSteps = []flowsdk.ArtifactKey{
	flowsdk.ArtifactPlan,
	flowsdk.ArtifactReview,
	flowsdk.ArtifactPhases,
}

// canonicalSteps returns the flow's step order for an item, by type. A flow is
// ONE binary running MANY steps in a fixed order — it never jumps to another
// flow. The list IS the flow's whole plan for that item, so both `do run` and
// `do status` derive against the same per-type list.
func canonicalSteps(it *flowsdk.Item) []flowsdk.ArtifactKey {
	if it.Type == flowsdk.ItemPlan {
		return planSteps
	}
	return taskSteps
}

// pending reports whether a step still needs to run for the item. A step is
// pending when its artifact is absent, or present-but-stale (an upstream change
// superseded it). A checklist entry the human marked NOT required is skipped
// (removing a requirement = skip that step / finalize without it).
//
// When the checklist is unseeded (no entry for the key yet), the flow treats its
// own canonical steps as required-by-default — so `do status` on a never-run
// item still shows the real plan. Derivation reads ONLY durable item state,
// never the ledger.
func pending(it *flowsdk.Item, key flowsdk.ArtifactKey) bool {
	a := it.Artifact(key)
	if a != nil && !a.Required {
		return false // human opted out of this step
	}
	if !it.ArtifactPresent(key) {
		return true
	}
	return a != nil && a.Stale
}

// firstPending returns the first canonical step still pending for the item, or
// "" if every step is present (and fresh) or skipped — i.e. the item is
// finalized. This is the artifact-derived "what runs next" before considering
// terminal stops.
func firstPending(it *flowsdk.Item) flowsdk.ArtifactKey {
	for _, key := range canonicalSteps(it) {
		if pending(it, key) {
			return key
		}
	}
	return ""
}

// terminalReason returns a non-empty reason when the tracker would stop
// dispatching further steps regardless of pending artifacts: the item is
// rejection-closed (a deliberate non-done close), blocked by another item, or
// awaiting a user answer. These are derived, not stored statuses. Returns "" when
// the flow may proceed.
func terminalReason(it *flowsdk.Item) string {
	switch {
	case it.IsRejectionClosed():
		return "rejection-closed: " + string(it.Status)
	case it.IsBlocked():
		return fmt.Sprintf("blocked by %v", it.ActiveBlockers)
	case it.NeedsAnswer():
		return "needs_answer: awaiting a user answer"
	default:
		return ""
	}
}

// deriveNext picks the single step `do run` would execute now, derived purely
// from durable item state (never the ledger). It returns ("", reason) when there
// is no next step: a terminal stop (terminalReason) or finalized (no pending
// artifact). `do status` uses the same firstPending/terminalReason helpers, so
// the two commands can never disagree about what is next.
func deriveNext(it *flowsdk.Item) (flowsdk.ArtifactKey, string) {
	if r := terminalReason(it); r != "" {
		return "", r
	}
	if it.FinalizedFlag {
		// The permanent finalized gate is set: every remaining required-but-missing
		// step is skipped, so there is nothing left for the flow to run.
		return "", "finalized: flag set — remaining required steps skipped"
	}
	next := firstPending(it)
	if next == "" {
		return "", "finalized: all required artifacts present"
	}
	return next, ""
}
