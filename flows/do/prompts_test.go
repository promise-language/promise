package main

import (
	"strings"
	"testing"

	flowsdk "djabi.dev/go/flow_sdk"
)

func TestPrompts_IncludeItemContext(t *testing.T) {
	it := &flowsdk.Item{ID: "T0042", Type: flowsdk.ItemTask, Title: "Fix the widget", Description: "the details", Plan: "step 1"}
	prompts := map[string]string{
		"plan":      planPrompt(it),
		"implement": implementPrompt(it),
		"review":    reviewPrompt(it),
		"coverage":  coveragePrompt(it),
		"inspect":   inspectPrompt(it),
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
	p := implementPrompt(it)
	if !strings.Contains(p, "do the thing") {
		t.Error("implement prompt should include the saved plan")
	}
	// The "derive from the description" fallback was removed: a missing plan is a
	// fail-fast precondition in stepImplementation, not something the prompt papers over.
	if strings.Contains(p, "no saved plan") {
		t.Error("implement prompt must not carry a missing-plan fallback")
	}
}

// TestPrompts_CarryEssentialSkillContent guards against the flow prompts drifting
// away from the substantive guidance in .claude/skills/* (the flow is the canonical
// executor, so the prompts must not silently lose essential skill content). The
// expected tokens are Promise-specific: the verify gate is `bin/verify --wasm`, and
// the build re-embeds modules via `bin/build`.
func TestPrompts_CarryEssentialSkillContent(t *testing.T) {
	it := &flowsdk.Item{ID: "T1", Type: flowsdk.ItemTask, Title: "t", Plan: "p"}
	prompts := map[string]string{
		"plan":      planPrompt(it),
		"implement": implementPrompt(it),
		"review":    reviewPrompt(it),
		"coverage":  coveragePrompt(it),
		"inspect":   inspectPrompt(it),
	}
	musts := map[string][]string{
		"plan":      {"reproduce", "blocked_by", "wontfix", "needs-attention", "parser → sema → ownership → codegen"},
		"implement": {"bin/build", "bin/verify --wasm", "allow_leaks", "mcp__tracker__create"},
		"review":    {"in full", "concurrency", "mcp__tracker__create", "bin/verify --wasm"},
		"coverage":  {"go tool cover", "bin/promise test -coverage", "mcp__tracker__create"},
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

func TestFixVerifyPrompt(t *testing.T) {
	it := &flowsdk.Item{ID: "T0042", Title: "t"}
	p := fixVerifyPrompt(it, "no successful bin/verify run found")
	for _, want := range []string{"T0042", "no successful bin/verify run found", "bin/verify --wasm", "allow_leaks", "Do NOT commit"} {
		if !strings.Contains(p, want) {
			t.Errorf("fixVerifyPrompt missing %q", want)
		}
	}
}

func TestItemHeader_OmitsEmptyDescription(t *testing.T) {
	withDesc := itemHeader(&flowsdk.Item{ID: "T1", Title: "t", Description: "d"})
	if !strings.Contains(withDesc, "Description:") {
		t.Error("expected the description section when present")
	}
	noDesc := itemHeader(&flowsdk.Item{ID: "T1", Title: "t"})
	if strings.Contains(noDesc, "Description:") {
		t.Error("empty description should be omitted")
	}
}
