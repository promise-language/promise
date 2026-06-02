package main

import (
	"strings"
	"testing"

	flowsdk "djabi.dev/go/flow_sdk"
)

// testVerifyCmd is the verify command threaded into prompts under test. It is the
// real Promise gate so the "bin/verify --wasm" content assertions below hold.
const testVerifyCmd = "bin/verify --wasm"

func TestPrompts_IncludeItemContext(t *testing.T) {
	it := &flowsdk.Item{ID: "T0042", Type: flowsdk.ItemTask, Title: "Fix the widget", Description: "the details", Plan: "step 1"}
	prompts := map[string]string{
		"plan":      planPrompt(it, testVerifyCmd),
		"implement": implementPrompt(it, testVerifyCmd),
		"review":    reviewPrompt(it, testVerifyCmd),
		"coverage":  coveragePrompt(it, testVerifyCmd),
		"summary":   summaryPrompt(it, testVerifyCmd),
		"inspect":   inspectPrompt(it, testVerifyCmd),
	}
	for name, p := range prompts {
		if !strings.Contains(p, "T0042") {
			t.Errorf("%s prompt missing item id", name)
		}
		if !strings.Contains(p, "Fix the widget") {
			t.Errorf("%s prompt missing item title", name)
		}
	}
	if !strings.Contains(prompts["implement"], "step 1") {
		t.Error("implement prompt should include the saved plan")
	}
	if !strings.Contains(prompts["coverage"], "COVERAGE:") {
		t.Error("coverage prompt should ask for the COVERAGE rating token")
	}
	if !strings.Contains(prompts["inspect"], "json") {
		t.Error("inspect prompt should ask for a JSON verdict block")
	}
}

func TestImplementPrompt_IncludesPlan(t *testing.T) {
	it := &flowsdk.Item{ID: "T1", Title: "t", Plan: "do the thing"}
	p := implementPrompt(it, testVerifyCmd)
	if !strings.Contains(p, "do the thing") {
		t.Error("implement prompt should include the saved plan")
	}
	// The implement step no longer writes the resolution summary — that moved to the
	// dedicated post-push summary step. The prompt must say so rather than ask the
	// agent to end with a resolution summary.
	if !strings.Contains(p, "dedicated step produces it") {
		t.Error("implement prompt should tell the agent the summary is a later step")
	}
}

func TestImplementFixPrompt_CarriesVerifyOutput(t *testing.T) {
	it := &flowsdk.Item{ID: "T7", Title: "fix it", Plan: "p"}
	out := "bin/verify --wasm\n--- FAIL: TestThing\nsome failure detail"
	p := implementFixPrompt(it, testVerifyCmd, out)
	if !strings.Contains(p, "T7") {
		t.Error("fix prompt should carry the item id")
	}
	if !strings.Contains(p, "bin/verify --wasm") {
		t.Error("fix prompt should reference the verify command")
	}
	if !strings.Contains(p, "TestThing") {
		t.Error("fix prompt should embed the verify output so the agent sees the failures")
	}
}

func TestRebaseConflictPrompt_ListsConflicts(t *testing.T) {
	it := &flowsdk.Item{ID: "T9", Title: "merge", Plan: "p"}
	p := rebaseConflictPrompt(it, testVerifyCmd, []string{"a.go", "b.pr"}, "CONFLICT (content): Merge conflict in a.go")
	if !strings.Contains(p, "a.go, b.pr") {
		t.Error("rebase conflict prompt should list the conflicted files")
	}
	if !strings.Contains(p, "git rebase --continue") {
		t.Error("rebase conflict prompt should instruct to continue the rebase")
	}
	if !strings.Contains(p, "bin/verify --wasm") {
		t.Error("rebase conflict prompt should re-verify the merged result")
	}
	// The post-merge re-verify is the one place a rebase can introduce a leak, so the
	// zero-leak requirement must survive here (it lives in the Promise-specific template,
	// not the domain-agnostic shared rebase partial).
	if !strings.Contains(p, "allow_leaks") {
		t.Error("rebase conflict prompt should carry the zero-leak requirement for the re-verify")
	}
}

// TestPrompts_CarryEssentialSkillContent guards against the flow prompts drifting
// away from the substantive guidance in .claude/skills/* (the flow is the canonical
// executor, so the prompts must not silently lose essential skill content). These
// are the Promise-specific markers — the compiler pipeline, the zero-leak policy,
// and Promise's build/verify/test commands.
func TestPrompts_CarryEssentialSkillContent(t *testing.T) {
	it := &flowsdk.Item{ID: "T1", Type: flowsdk.ItemTask, Title: "t", Plan: "p"}
	prompts := map[string]string{
		"plan":      planPrompt(it, testVerifyCmd),
		"implement": implementPrompt(it, testVerifyCmd),
		"review":    reviewPrompt(it, testVerifyCmd),
		"coverage":  coveragePrompt(it, testVerifyCmd),
		"summary":   summaryPrompt(it, testVerifyCmd),
		"inspect":   inspectPrompt(it, testVerifyCmd),
	}
	musts := map[string][]string{
		"plan":      {"reproduce", "blocked_by", "wontfix", "needs-attention", "parser → sema → ownership → codegen"},
		"implement": {"bin/verify --wasm", "allow_leaks", "bin/build", "mcp__tracker__create"},
		"review":    {"in full", "concurrency", "mcp__tracker__create", "bin/verify --wasm", "zero tolerance"},
		"coverage":  {"go tool cover", "bin/promise test -coverage", "mcp__tracker__create"},
		"summary":   {"READ-ONLY", "committed, and pushed"},
		"inspect":   {"read-only", "do NOT run", "file and line"},
	}
	for name, want := range musts {
		for _, m := range want {
			if !strings.Contains(prompts[name], m) {
				t.Errorf("%s prompt missing essential skill content %q", name, m)
			}
		}
	}
}

// TestPlanPrompt_AlreadyImplementedGuard verifies the plan prompt carries the
// shared already-implemented/duplicate resolution branch — the fix for the stall
// where a "it's already done" plan with no close-out leaves the implement step
// producing an empty diff on every retry.
func TestPlanPrompt_AlreadyImplementedGuard(t *testing.T) {
	p := planPrompt(&flowsdk.Item{ID: "T1", Type: flowsdk.ItemTask, Title: "t"}, testVerifyCmd)
	for _, want := range []string{"ALREADY IMPLEMENTED", "do NOT produce a plan", "empty diff"} {
		if !strings.Contains(p, want) {
			t.Errorf("plan prompt missing already-implemented guidance %q", want)
		}
	}
}

func TestReviewPrompt_PlanItemVariant(t *testing.T) {
	plan := reviewPrompt(&flowsdk.Item{ID: "P1", Type: flowsdk.ItemPlan, Title: "a plan"}, testVerifyCmd)
	if !strings.Contains(plan, "review THIS PLAN") {
		t.Error("plan-item review should critique the plan itself")
	}
	task := reviewPrompt(&flowsdk.Item{ID: "T1", Type: flowsdk.ItemTask, Title: "a task"}, testVerifyCmd)
	if strings.Contains(task, "review THIS PLAN") {
		t.Error("task-item review should be the code review, not the plan review")
	}
}

func TestItemHeader_OmitsEmptyDescription(t *testing.T) {
	withDesc := planPrompt(&flowsdk.Item{ID: "T1", Title: "t", Description: "the details"}, testVerifyCmd)
	if !strings.Contains(withDesc, "Description:") || !strings.Contains(withDesc, "the details") {
		t.Error("expected the description section when present")
	}
	noDesc := planPrompt(&flowsdk.Item{ID: "T1", Title: "t"}, testVerifyCmd)
	if strings.Contains(noDesc, "Description:") {
		t.Error("empty description should be omitted")
	}
}

// TestVerifyCmd_ThreadedNotHardcoded confirms the verify command in prompts comes
// from the passed-in value (App.VerifyCmd via StepCtx.VerifyCmd()), not a baked-in
// literal: a custom command must appear and the default must not.
func TestVerifyCmd_ThreadedNotHardcoded(t *testing.T) {
	it := &flowsdk.Item{ID: "T1", Title: "t", Plan: "p"}
	p := implementPrompt(it, "make ci-check")
	if !strings.Contains(p, "make ci-check") {
		t.Error("implement prompt should use the threaded verify command")
	}
	if strings.Contains(p, "bin/verify --wasm") {
		t.Error("implement prompt should NOT hardcode the default verify command when a custom one is passed")
	}
}
