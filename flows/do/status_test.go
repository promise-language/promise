package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	flowsdk "djabi.dev/go/flow_sdk"
)

// stateOf returns the projected state of a step in a FlowStatus.
func stateOf(s flowsdk.FlowStatus, key flowsdk.ArtifactKey) flowsdk.StepState {
	for _, st := range s.Steps {
		if st.Step == flowsdk.StepName(key) {
			return st.State
		}
	}
	return ""
}

func TestBuildFlowStatus_MidFlow(t *testing.T) {
	it := itemWith(flowsdk.ArtifactPlan, flowsdk.ArtifactImplementation)
	s := buildFlowStatus(it)
	if len(s.Steps) != len(taskSteps) {
		t.Fatalf("got %d steps, want %d", len(s.Steps), len(taskSteps))
	}
	if got := stateOf(s, flowsdk.ArtifactPlan); got != flowsdk.StepStateCompleted {
		t.Errorf("plan = %q, want completed", got)
	}
	if got := stateOf(s, flowsdk.ArtifactImplementation); got != flowsdk.StepStateCompleted {
		t.Errorf("implementation = %q, want completed", got)
	}
	if got := stateOf(s, flowsdk.ArtifactReview); got != flowsdk.StepStateNext {
		t.Errorf("review = %q, want next", got)
	}
	if got := stateOf(s, flowsdk.ArtifactCoverage); got != flowsdk.StepStatePending {
		t.Errorf("coverage = %q, want pending", got)
	}
	if s.Terminal != "" {
		t.Errorf("Terminal = %q, want empty mid-flow", s.Terminal)
	}
	// Exactly one step is marked next.
	n := 0
	for _, st := range s.Steps {
		if st.State == flowsdk.StepStateNext {
			n++
		}
	}
	if n != 1 {
		t.Errorf("got %d next steps, want exactly 1", n)
	}
}

func TestBuildFlowStatus_Finalized(t *testing.T) {
	s := buildFlowStatus(itemWith(taskSteps...))
	for _, st := range s.Steps {
		if st.State != flowsdk.StepStateCompleted {
			t.Errorf("%s = %q, want completed", st.Step, st.State)
		}
	}
	if s.Terminal == "" {
		t.Error("expected a Terminal reason when finalized")
	}
}

func TestBuildFlowStatus_FinalizedFlag(t *testing.T) {
	// FinalizedFlag set while the flow's artifact view still sees steps missing:
	// present steps stay completed, every remaining required step is skipped
	// ("finalized"), no step is next, and Terminal names the flag gate.
	it := itemWith(flowsdk.ArtifactPlan)
	it.FinalizedFlag = true
	s := buildFlowStatus(it)
	if got := stateOf(s, flowsdk.ArtifactPlan); got != flowsdk.StepStateCompleted {
		t.Errorf("plan = %q, want completed", got)
	}
	if got := stateOf(s, flowsdk.ArtifactImplementation); got != flowsdk.StepStateSkipped {
		t.Errorf("implementation = %q, want skipped (finalized flag)", got)
	}
	for _, st := range s.Steps {
		if st.State == flowsdk.StepStateNext {
			t.Errorf("step %s marked next while finalized flag is set", st.Step)
		}
	}
	if !strings.Contains(s.Terminal, "finalized: flag set") {
		t.Errorf("Terminal = %q, want flag-set finalized reason", s.Terminal)
	}
}

func TestBuildFlowStatus_TerminalMarksBlocked(t *testing.T) {
	it := itemWith(flowsdk.ArtifactPlan)
	it.Questions = []flowsdk.Question{{ID: "q1", AgentQuestion: flowsdk.AgentQuestion{Text: "?"}}}
	s := buildFlowStatus(it)
	if got := stateOf(s, flowsdk.ArtifactImplementation); got != flowsdk.StepStateBlocked {
		t.Errorf("implementation = %q, want blocked (needs_answer)", got)
	}
	// No step should be marked next when terminal.
	for _, st := range s.Steps {
		if st.State == flowsdk.StepStateNext {
			t.Errorf("step %s marked next while terminal", st.Step)
		}
	}
	if !strings.Contains(s.Terminal, "needs_answer") {
		t.Errorf("Terminal = %q, want needs_answer", s.Terminal)
	}
}

func TestBuildFlowStatus_SkippedNotRequired(t *testing.T) {
	it := itemWith(flowsdk.ArtifactPlan, flowsdk.ArtifactImplementation, flowsdk.ArtifactReview)
	for i := range it.Artifacts {
		if it.Artifacts[i].Key == flowsdk.ArtifactCoverage {
			it.Artifacts[i].Required = false
		}
	}
	s := buildFlowStatus(it)
	if got := stateOf(s, flowsdk.ArtifactCoverage); got != flowsdk.StepStateSkipped {
		t.Errorf("coverage = %q, want skipped", got)
	}
	if got := stateOf(s, flowsdk.ArtifactCommit); got != flowsdk.StepStateNext {
		t.Errorf("commit = %q, want next (coverage skipped)", got)
	}
}

func TestBuildFlowStatus_StaleIsNextWithReason(t *testing.T) {
	it := itemWith(taskSteps...)
	for i := range it.Artifacts {
		if it.Artifacts[i].Key == flowsdk.ArtifactReview {
			it.Artifacts[i].Stale = true
		}
	}
	s := buildFlowStatus(it)
	if got := stateOf(s, flowsdk.ArtifactReview); got != flowsdk.StepStateNext {
		t.Fatalf("review = %q, want next (stale)", got)
	}
	for _, st := range s.Steps {
		if st.Step == flowsdk.StepName(flowsdk.ArtifactReview) && st.Reason == "" {
			t.Error("expected a reason on the stale next step")
		}
	}
}

func TestRenderStatusJSON_RoundTrips(t *testing.T) {
	s := buildFlowStatus(itemWith(flowsdk.ArtifactPlan))
	var buf bytes.Buffer
	if err := renderStatusJSON(&buf, s); err != nil {
		t.Fatal(err)
	}
	var got flowsdk.FlowStatus
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("status JSON did not round-trip: %v", err)
	}
	if got.ItemID != s.ItemID || len(got.Steps) != len(s.Steps) {
		t.Errorf("round-trip mismatch: %+v vs %+v", got, s)
	}
}

func TestStatusRenderMode(t *testing.T) {
	if statusRenderMode([]string{"--json"}, true) != true {
		t.Error("--json should force JSON even on a TTY")
	}
	if statusRenderMode([]string{"--text"}, false) != false {
		t.Error("--text should force text even when piped")
	}
	if statusRenderMode(nil, true) != false {
		t.Error("a TTY with no flag should render text")
	}
	if statusRenderMode(nil, false) != true {
		t.Error("piped output with no flag should render JSON")
	}
}

func TestRenderStatusText_IncludesSteps(t *testing.T) {
	var buf bytes.Buffer
	renderStatusText(&buf, buildFlowStatus(itemWith(flowsdk.ArtifactPlan)))
	out := buf.String()
	if !strings.Contains(out, "plan") || !strings.Contains(out, "implementation") {
		t.Errorf("text output missing steps:\n%s", out)
	}
}

func TestRenderStatusText_AllGlyphsAndTerminal(t *testing.T) {
	// completed (plan) + skipped (coverage not-required) + blocked (needs_answer)
	// + a terminal line — exercises every stepMark branch.
	it := itemWith(flowsdk.ArtifactPlan)
	for i := range it.Artifacts {
		if it.Artifacts[i].Key == flowsdk.ArtifactCoverage {
			it.Artifacts[i].Required = false
		}
	}
	it.Questions = []flowsdk.Question{{ID: "q1", AgentQuestion: flowsdk.AgentQuestion{Text: "?"}}}
	var buf bytes.Buffer
	renderStatusText(&buf, buildFlowStatus(it))
	out := buf.String()
	for _, glyph := range []string{"✓", "✗", "-", "·"} {
		if !strings.Contains(out, glyph) {
			t.Errorf("render missing glyph %q:\n%s", glyph, out)
		}
	}
	if !strings.Contains(out, "terminal:") {
		t.Errorf("render missing terminal line:\n%s", out)
	}
}

func TestStepMark_NextGlyph(t *testing.T) {
	if stepMark(flowsdk.StepStateNext) != "▶" {
		t.Error("next glyph mismatch")
	}
}
