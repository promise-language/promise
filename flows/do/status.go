package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	flowsdk "djabi.dev/go/flow_sdk"
)

// buildFlowStatus projects the flow's whole step plan for an item from durable
// item state alone — the same firstPending/terminalReason derivation `do run`
// uses, NEVER the ledger. Every canonical step gets a state:
//
//   - skipped   — the human marked the checklist entry not-required
//   - completed — the artifact is present and fresh
//   - next      — the first pending step, and the item is not terminal
//   - blocked   — the first pending step, but the item is terminal (no next)
//   - pending   — a later pending step
//
// The one `next` (or `blocked`) step is marked; FlowStatus.Terminal carries the
// reason when there is no next step.
func buildFlowStatus(it *flowsdk.Item) flowsdk.FlowStatus {
	terminal := terminalReason(it)
	first := firstPending(it) // first pending step (independent of terminal)

	keys := canonicalSteps(it)
	steps := make([]flowsdk.FlowStep, 0, len(keys))
	for _, key := range keys {
		st := flowsdk.FlowStep{Step: flowsdk.StepName(key)}
		a := it.Artifact(key)
		switch {
		case a != nil && !a.Required:
			st.State = flowsdk.StepStateSkipped
			st.Reason = "not required"
		case it.ArtifactPresent(key) && !(a != nil && a.Stale):
			st.State = flowsdk.StepStateCompleted
		case it.FinalizedFlag:
			// Permanent finalized gate set → remaining required-but-missing steps
			// are skipped (the flow will not run them).
			st.State = flowsdk.StepStateSkipped
			st.Reason = "finalized"
		case key == first && terminal == "":
			st.State = flowsdk.StepStateNext
			if a != nil && a.Stale {
				st.Reason = "stale — re-run"
			}
		case key == first && terminal != "":
			st.State = flowsdk.StepStateBlocked
			st.Reason = terminal
		default:
			st.State = flowsdk.StepStatePending
			if a != nil && a.Stale {
				st.Reason = "stale"
			}
		}
		steps = append(steps, st)
	}

	status := flowsdk.FlowStatus{
		Flow:   flowName,
		ItemID: it.ID,
		Steps:  steps,
	}
	if terminal != "" {
		status.Terminal = terminal
	} else if it.FinalizedFlag {
		status.Terminal = "finalized: flag set (remaining required steps skipped)"
	} else if first == "" {
		status.Terminal = "finalized: all required artifacts present"
	}
	return status
}

// renderStatusText writes a human-readable checklist of the step plan (used on a
// TTY). JSON is the canonical form (renderStatusJSON); this is a convenience.
func renderStatusText(w io.Writer, s flowsdk.FlowStatus) {
	fmt.Fprintf(w, "flow %s — item %s\n", s.Flow, s.ItemID)
	for _, st := range s.Steps {
		mark := stepMark(st.State)
		line := fmt.Sprintf("  %s %-15s %s", mark, st.Step, st.State)
		if st.Reason != "" {
			line += "  (" + st.Reason + ")"
		}
		fmt.Fprintln(w, line)
	}
	if s.Terminal != "" {
		fmt.Fprintf(w, "  → terminal: %s\n", s.Terminal)
	}
}

// stepMark returns a single-character glyph for a step state.
func stepMark(state flowsdk.StepState) string {
	switch state {
	case flowsdk.StepStateCompleted:
		return "✓"
	case flowsdk.StepStateNext:
		return "▶"
	case flowsdk.StepStateSkipped:
		return "-"
	case flowsdk.StepStateBlocked:
		return "✗"
	default:
		return "·"
	}
}

// renderStatusJSON writes the canonical JSON form of the status.
func renderStatusJSON(w io.Writer, s flowsdk.FlowStatus) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// statusRenderMode decides JSON vs text output from explicit flags and TTY-ness.
// JSON is canonical and is the default for non-TTY (piped/captured) output; a TTY
// gets the checklist unless --json is given.
func statusRenderMode(args []string, isTTY bool) (jsonOut bool) {
	for _, a := range args {
		switch strings.TrimSpace(a) {
		case "--json":
			return true
		case "--text":
			return false
		}
	}
	return !isTTY
}
