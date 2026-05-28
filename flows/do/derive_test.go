package main

import (
	"strings"
	"testing"

	flowsdk "djabi.dev/go/flow_sdk"
)

// itemWith builds an item whose listed artifacts are present (content fields set)
// and whose checklist is seeded Required for every canonical step, unless
// overridden by opts.
func itemWith(present ...flowsdk.ArtifactKey) *flowsdk.Item {
	it := &flowsdk.Item{ID: "T0001", Status: flowsdk.StatusOpen}
	for _, k := range taskSteps {
		it.Artifacts = append(it.Artifacts, flowsdk.ItemArtifact{Key: k, Required: true})
	}
	for _, k := range present {
		setArtifactPresent(it, k)
	}
	return it
}

// setArtifactPresent sets the content field(s) that make ArtifactPresent(key) true.
func setArtifactPresent(it *flowsdk.Item, key flowsdk.ArtifactKey) {
	switch key {
	case flowsdk.ArtifactPlan:
		it.Plan = "a plan"
	case flowsdk.ArtifactImplementation:
		it.Patches = []flowsdk.ItemPatch{{Hash: "h1"}}
	case flowsdk.ArtifactReview:
		it.ReviewSummary = "reviewed"
	case flowsdk.ArtifactCoverage:
		it.TestCoverage = flowsdk.CoverageAdequate
	case flowsdk.ArtifactCommit:
		it.CommitHash = "c0ffee"
	case flowsdk.ArtifactPush:
		it.PushedHash = "c0ffee"
	case flowsdk.ArtifactInspection:
		it.Inspection = &flowsdk.Inspection{Verdict: flowsdk.VerdictPass}
	case flowsdk.ArtifactPhases:
		it.Phases = []flowsdk.ItemID{"T0002", "T0003"}
	}
}

func TestDeriveNext_Progression(t *testing.T) {
	tests := []struct {
		name    string
		present []flowsdk.ArtifactKey
		want    flowsdk.ArtifactKey
	}{
		{"nothing → plan", nil, flowsdk.ArtifactPlan},
		{"plan → implementation", []flowsdk.ArtifactKey{flowsdk.ArtifactPlan}, flowsdk.ArtifactImplementation},
		{"plan+impl → review", []flowsdk.ArtifactKey{flowsdk.ArtifactPlan, flowsdk.ArtifactImplementation}, flowsdk.ArtifactReview},
		{"…+review → coverage", []flowsdk.ArtifactKey{flowsdk.ArtifactPlan, flowsdk.ArtifactImplementation, flowsdk.ArtifactReview}, flowsdk.ArtifactCoverage},
		{"…+coverage → commit", []flowsdk.ArtifactKey{flowsdk.ArtifactPlan, flowsdk.ArtifactImplementation, flowsdk.ArtifactReview, flowsdk.ArtifactCoverage}, flowsdk.ArtifactCommit},
		{"…+commit → push", []flowsdk.ArtifactKey{flowsdk.ArtifactPlan, flowsdk.ArtifactImplementation, flowsdk.ArtifactReview, flowsdk.ArtifactCoverage, flowsdk.ArtifactCommit}, flowsdk.ArtifactPush},
		{"…+push → inspection", []flowsdk.ArtifactKey{flowsdk.ArtifactPlan, flowsdk.ArtifactImplementation, flowsdk.ArtifactReview, flowsdk.ArtifactCoverage, flowsdk.ArtifactCommit, flowsdk.ArtifactPush}, flowsdk.ArtifactInspection},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := itemWith(tt.present...)
			next, term := deriveNext(it)
			if term != "" {
				t.Fatalf("unexpected terminal: %q", term)
			}
			if next != tt.want {
				t.Fatalf("deriveNext = %q, want %q", next, tt.want)
			}
		})
	}
}

func TestDeriveNext_Finalized(t *testing.T) {
	it := itemWith(taskSteps...)
	next, term := deriveNext(it)
	if next != "" {
		t.Fatalf("expected no next step when finalized, got %q", next)
	}
	if term == "" {
		t.Fatal("expected a terminal reason when finalized")
	}
	if !it.Finalized() {
		t.Fatal("expected item Finalized()")
	}
}

func TestDeriveNext_FinalizedFlag(t *testing.T) {
	// The tracker's permanent finalized gate refuses further runs even when the
	// flow's own artifact view still sees required steps missing (e.g. no patch
	// captured) — the flag is authoritative over firstPending.
	it := itemWith(flowsdk.ArtifactPlan) // implementation onward still "missing"
	it.FinalizedFlag = true
	next, term := deriveNext(it)
	if next != "" {
		t.Fatalf("expected no next step when FinalizedFlag set, got %q", next)
	}
	if !strings.Contains(term, "finalized: flag set") {
		t.Fatalf("terminal = %q, want the flag-set finalized reason", term)
	}
}

func TestDeriveNext_UnseededShowsPlan(t *testing.T) {
	// An item that was never run has no checklist — the flow still treats its
	// canonical steps as required, so the plan is next.
	it := &flowsdk.Item{ID: "T0002", Status: flowsdk.StatusOpen}
	next, term := deriveNext(it)
	if term != "" {
		t.Fatalf("unexpected terminal: %q", term)
	}
	if next != flowsdk.ArtifactPlan {
		t.Fatalf("deriveNext = %q, want plan", next)
	}
}

func TestDeriveNext_TerminalShortCircuits(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*flowsdk.Item)
		want   string // substring of the terminal reason
	}{
		{"rejection-closed", func(it *flowsdk.Item) { it.Status = flowsdk.StatusWontfix }, "rejection-closed"},
		{"blocked", func(it *flowsdk.Item) { it.ActiveBlockers = []flowsdk.ItemID{"T0099"} }, "blocked"},
		{"needs_answer", func(it *flowsdk.Item) {
			it.Questions = []flowsdk.Question{{ID: "q1", AgentQuestion: flowsdk.AgentQuestion{Text: "?"}}}
		}, "needs_answer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := itemWith(flowsdk.ArtifactPlan) // mid-flow
			tt.mutate(it)
			next, term := deriveNext(it)
			if next != "" {
				t.Fatalf("expected no next step, got %q", next)
			}
			if term == "" || !contains(term, tt.want) {
				t.Fatalf("terminal = %q, want substring %q", term, tt.want)
			}
		})
	}
}

func TestDeriveNext_DoneNotTerminalWhilePending(t *testing.T) {
	// `done` is NOT rejection — a done item with pending artifacts is re-admitted
	// so finalization runs. deriveNext must still return the next pending step.
	it := itemWith(flowsdk.ArtifactPlan, flowsdk.ArtifactImplementation)
	it.Status = flowsdk.StatusDone
	next, term := deriveNext(it)
	if term != "" {
		t.Fatalf("done+pending should not be terminal, got %q", term)
	}
	if next != flowsdk.ArtifactReview {
		t.Fatalf("deriveNext = %q, want review", next)
	}
}

func TestPending_StaleReopens(t *testing.T) {
	it := itemWith(taskSteps...) // all present
	// Mark review stale: it becomes pending again, and it is the next step.
	for i := range it.Artifacts {
		if it.Artifacts[i].Key == flowsdk.ArtifactReview {
			it.Artifacts[i].Stale = true
		}
	}
	if !pending(it, flowsdk.ArtifactReview) {
		t.Fatal("stale review should be pending")
	}
	next, term := deriveNext(it)
	if term != "" {
		t.Fatalf("unexpected terminal: %q", term)
	}
	if next != flowsdk.ArtifactReview {
		t.Fatalf("deriveNext = %q, want review (stale)", next)
	}
}

func TestPending_NotRequiredSkipped(t *testing.T) {
	it := itemWith(flowsdk.ArtifactPlan, flowsdk.ArtifactImplementation)
	// Human removes the coverage requirement → it is skipped; next jumps past it.
	for i := range it.Artifacts {
		if it.Artifacts[i].Key == flowsdk.ArtifactCoverage {
			it.Artifacts[i].Required = false
		}
	}
	if pending(it, flowsdk.ArtifactCoverage) {
		t.Fatal("not-required coverage should not be pending")
	}
	// review is next (still pending); after review the next pending skips coverage.
	withReview := itemWith(flowsdk.ArtifactPlan, flowsdk.ArtifactImplementation, flowsdk.ArtifactReview)
	for i := range withReview.Artifacts {
		if withReview.Artifacts[i].Key == flowsdk.ArtifactCoverage {
			withReview.Artifacts[i].Required = false
		}
	}
	next, _ := deriveNext(withReview)
	if next != flowsdk.ArtifactCommit {
		t.Fatalf("deriveNext = %q, want commit (coverage skipped)", next)
	}
}

// planItem builds a plan-type item whose checklist is seeded for the plan steps.
func planItem(present ...flowsdk.ArtifactKey) *flowsdk.Item {
	it := &flowsdk.Item{ID: "P0001", Type: flowsdk.ItemPlan, Status: flowsdk.StatusOpen}
	for _, k := range planSteps {
		it.Artifacts = append(it.Artifacts, flowsdk.ItemArtifact{Key: k, Required: true})
	}
	for _, k := range present {
		setArtifactPresent(it, k)
	}
	return it
}

func TestDeriveNext_Plan(t *testing.T) {
	// A plan runs ONLY plan → review → phases — never the code lifecycle.
	if next, _ := deriveNext(planItem()); next != flowsdk.ArtifactPlan {
		t.Errorf("first plan step = %q, want plan", next)
	}
	if next, _ := deriveNext(planItem(flowsdk.ArtifactPlan)); next != flowsdk.ArtifactReview {
		t.Errorf("after plan, next = %q, want review", next)
	}
	if next, _ := deriveNext(planItem(flowsdk.ArtifactPlan, flowsdk.ArtifactReview)); next != flowsdk.ArtifactPhases {
		t.Errorf("after review, next = %q, want phases", next)
	}
	// Plan + review + phases filed → finalized; a plan must never derive implementation/etc.
	done := planItem(flowsdk.ArtifactPlan, flowsdk.ArtifactReview, flowsdk.ArtifactPhases)
	if next, reason := deriveNext(done); next != "" {
		t.Errorf("plan should finalize after phases, got next=%q reason=%q", next, reason)
	}
	if done.HasPendingArtifacts() {
		t.Error("a finalized plan (plan+review+phases) should not have pending artifacts")
	}
}

func TestPlan_PhasesArtifactIsFiledListNotActivePhases(t *testing.T) {
	// Presence is the stable FILED list, so a finalized plan stays present even
	// when every phase later completes (ActivePhases would shrink to empty).
	it := planItem(flowsdk.ArtifactPlan, flowsdk.ArtifactPhases)
	it.ActivePhases = nil // all phases done — derived view is empty
	if !it.ArtifactPresent(flowsdk.ArtifactPhases) {
		t.Error("phases artifact must stay present once filed, regardless of ActivePhases")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
