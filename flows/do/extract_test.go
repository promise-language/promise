package main

import (
	"strings"
	"testing"

	flowsdk "djabi.dev/go/flow_sdk"
)

func TestExtractCoverage(t *testing.T) {
	tests := []struct {
		msg  string
		want flowsdk.TestCoverage
	}{
		{"added tests.\nCOVERAGE: adequate", flowsdk.CoverageAdequate},
		{"COVERAGE: insufficient", flowsdk.CoverageInsufficient},
		{"COVERAGE: none", flowsdk.CoverageNone},
		{"coverage: ADEQUATE", flowsdk.CoverageAdequate},                       // case-insensitive
		{"no token here", flowsdk.CoverageAdequate},                            // default
		{"COVERAGE: none\nlater COVERAGE: adequate", flowsdk.CoverageAdequate}, // last wins
	}
	for _, tt := range tests {
		if got := extractCoverage(tt.msg); got != tt.want {
			t.Errorf("extractCoverage(%q) = %q, want %q", tt.msg, got, tt.want)
		}
	}
}

func TestExtractInspection_ParsesJSON(t *testing.T) {
	msg := "Looks good overall.\n\n```json\n" + `{
  "verdict": "concerns",
  "quality": "good",
  "completeness": "full",
  "summary": "Solid, one nit.",
  "tags": ["codegen"],
  "suggestions": [
    {"title": "Add a retry test", "type": "task", "rationale": "edge case", "key": "retry-test", "priority": "medium"},
    {"title": "", "type": "task"}
  ]
}` + "\n```\n"
	insp, sugs := extractInspection(msg, "inspector")
	if insp.Verdict != flowsdk.VerdictConcerns {
		t.Errorf("verdict = %q, want concerns", insp.Verdict)
	}
	if insp.Quality != flowsdk.QualityGood || insp.Completeness != flowsdk.CompletenessFull {
		t.Errorf("quality/completeness = %q/%q", insp.Quality, insp.Completeness)
	}
	if insp.InspectedBy != "inspector" {
		t.Errorf("inspected_by = %q, want inspector", insp.InspectedBy)
	}
	if len(insp.Tags) != 1 || insp.Tags[0] != "codegen" {
		t.Errorf("tags = %v", insp.Tags)
	}
	if len(sugs) != 1 { // the empty-title suggestion is dropped
		t.Fatalf("got %d suggestions, want 1", len(sugs))
	}
	if sugs[0].Key != "retry-test" || sugs[0].Source != flowsdk.StepName(flowsdk.ArtifactInspection) {
		t.Errorf("suggestion = %+v", sugs[0])
	}
}

func TestExtractInspection_LastBlockWins(t *testing.T) {
	// An agent that echoes the template block first, then emits the real verdict.
	msg := "Here is the shape:\n```json\n" + `{"verdict": "pass", "summary": "template"}` + "\n```\n" +
		"And my actual verdict:\n```json\n" + `{"verdict": "fail", "summary": "real"}` + "\n```\n"
	insp, _ := extractInspection(msg, "inspector")
	if insp.Verdict != flowsdk.VerdictFail {
		t.Errorf("verdict = %q, want fail (last block)", insp.Verdict)
	}
	if !strings.Contains(string(insp.Summary), "real") {
		t.Errorf("summary = %q, want the last block's summary", insp.Summary)
	}
}

func TestExtractInspection_FallbackWhenNoJSON(t *testing.T) {
	insp, sugs := extractInspection("Just prose, no verdict block.", "inspector")
	if insp.Verdict != flowsdk.VerdictConcerns {
		t.Errorf("fallback verdict = %q, want concerns", insp.Verdict)
	}
	if !strings.Contains(string(insp.Summary), "Just prose") {
		t.Errorf("fallback summary = %q, want the raw message", insp.Summary)
	}
	if len(sugs) != 0 {
		t.Errorf("got %d suggestions, want 0", len(sugs))
	}
}

func TestExtractInspection_FallbackOnMalformedJSON(t *testing.T) {
	insp, _ := extractInspection("```json\n{not valid}\n```", "inspector")
	if insp.Verdict != flowsdk.VerdictConcerns {
		t.Errorf("verdict = %q, want concerns fallback", insp.Verdict)
	}
}

func TestGitChanged(t *testing.T) {
	a := &flowsdk.GitStatus{LastCommit: "x", Modified: 0}
	b := &flowsdk.GitStatus{LastCommit: "x", Modified: 0}
	if gitChanged(a, b) {
		t.Error("identical status should report no change")
	}
	if !gitChanged(a, &flowsdk.GitStatus{LastCommit: "x", Modified: 2}) {
		t.Error("modified-count change should report a change")
	}
	if !gitChanged(a, &flowsdk.GitStatus{LastCommit: "y"}) {
		t.Error("new commit should report a change")
	}
	if gitChanged(nil, nil) {
		t.Error("nil/nil should report no change")
	}
}

func TestCommitPushResultHelpers(t *testing.T) {
	if !isNothingToCommit(&flowsdk.ArenaResult{Error: "nothing to commit, working tree clean"}) {
		t.Error("should detect nothing-to-commit")
	}
	if isNothingToCommit(&flowsdk.ArenaResult{Error: "fatal: bad object"}) {
		t.Error("real error is not nothing-to-commit")
	}
	if !isUpToDate(&flowsdk.ArenaResult{Output: "Everything up-to-date"}) {
		t.Error("should detect up-to-date push")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("  hello  ", 100); got != "hello" {
		t.Errorf("truncate trim = %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("truncate clip = %q", got)
	}
}

func TestCommitMessage(t *testing.T) {
	it := &flowsdk.Item{ID: "T0042", Title: "Fix the widget"}
	if got := commitMessage(it); got != "T0042: Fix the widget" {
		t.Errorf("commitMessage = %q, want \"T0042: Fix the widget\"", got)
	}
}
