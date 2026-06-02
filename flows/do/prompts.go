package main

import (
	"embed"
	"strings"
	"text/template"

	flowsdk "djabi.dev/go/flow_sdk"
	"github.com/promise-language/flow/prompt"
)

// prompts.go builds the per-step instructions the flow drives the agent with.
// They carry the ESSENTIAL content of the Promise project's .claude/skills/{plan,
// implement,review,coverage,inspect}; the lifecycle/harness mechanics those skills
// also carry (capture-patch, mark-done, ask-tool selection, "run verify in the
// foreground") are intentionally omitted because the FLOW owns them — this keeps
// the flow canonical while the skills stay the hand-run path. Keep the two in
// sync: when a skill's substantive guidance changes, update the matching template
// in templates/.
//
// Composition: each prompt is a Go text/template in templates/*.tmpl executed
// against a doContext. The DOMAIN-AGNOSTIC fragments (item header, ask-the-user
// guidance, plan feasibility/already-implemented resolution, rebase-conflict
// resolution, defer-commit reminder) live in the shared flow/prompt package as
// reusable partials; prompt.Context.Render() pre-renders them into string fields
// that the templates reference as {{.ItemHeader}}, {{.PlanStepResolution}}, etc.
// The Promise-specific bodies (compiler pipeline, zero-leak policy, bin/build,
// Promise test idioms) live here in templates/. The verify command is NOT
// hardcoded — it is threaded from App.VerifyCmd via StepCtx.VerifyCmd() into
// {{.VerifyCmd}}, so the shared partials and the bodies refer to the same value.
//
// Division of labor: the AGENT does the thinking/coding for the step; the FLOW
// records the durable artifact from the turn output (it is the artifact writer).
// The agent may use the tracker MCP for genuine domain decisions — asking the
// user (mcp__tracker__ask_user_question) or closing an infeasible/blocked item —
// which the flow detects via a post-turn item re-fetch.

//go:embed templates/*.tmpl
var templateFS embed.FS

// stepTmpl holds every per-step template, keyed by its base filename (e.g.
// "plan.tmpl"). Parsed once at init; a malformed template panics the program at
// startup rather than at first render.
var stepTmpl = template.Must(template.New("do").ParseFS(templateFS, "templates/*.tmpl"))

// doContext is the data the step templates execute against. It embeds the shared
// prompt.Context (raw item fields + pre-rendered partials) and adds the
// per-step fields only the Promise bodies need.
type doContext struct {
	prompt.Context
	Plan         string // implement: the recorded plan
	VerifyOutput string // implement_fix: the tail of the failing verify run
	Conflicts    string // rebase_conflict: comma-joined conflicted paths
	RebaseOutput string // rebase_conflict: the raw rebase output (truncated)
}

// baseContext builds a doContext for an item with the shared partials already
// rendered. verifyCmd is App.VerifyCmd (via StepCtx.VerifyCmd()); it flows into
// {{.VerifyCmd}} everywhere a prompt references the verify gate.
func baseContext(it *flowsdk.Item, verifyCmd string) doContext {
	c := prompt.Context{
		ItemID:          string(it.ID),
		ItemType:        string(it.Type),
		ItemTitle:       it.Title,
		ItemDescription: string(it.Description),
		VerifyCmd:       verifyCmd,
	}
	// Render fills ItemHeader/AskGuidance/PlanStepResolution/RebaseResolution/
	// DeferCommit. An error here is a programmer bug (static templates), so panic
	// — it is caught the moment any prompt test runs.
	if err := c.Render(); err != nil {
		panic("do: render shared prompt partials: " + err.Error())
	}
	return doContext{Context: c}
}

// render executes the named step template against dc. A render error is a
// programmer bug (the templates are static and exercised by prompts_test.go), so
// it panics rather than returning an error the caller would have to handle.
func render(name string, dc doContext) string {
	var b strings.Builder
	if err := stepTmpl.ExecuteTemplate(&b, name, dc); err != nil {
		panic("do: render prompt " + name + ": " + err.Error())
	}
	return strings.TrimSpace(b.String()) + "\n"
}

func planPrompt(it *flowsdk.Item, verifyCmd string) string {
	return render("plan.tmpl", baseContext(it, verifyCmd))
}

func phasesPrompt(it *flowsdk.Item, verifyCmd string) string {
	return render("phases.tmpl", baseContext(it, verifyCmd))
}

func implementPrompt(it *flowsdk.Item, verifyCmd string) string {
	// The plan is a guaranteed prerequisite (stepImplementation fails fast if it
	// is missing), so the prompt always carries a real plan.
	dc := baseContext(it, verifyCmd)
	dc.Plan = strings.TrimSpace(string(it.Plan))
	return render("implement.tmpl", dc)
}

// implementFixPrompt re-prompts the implementing agent (in the same session) when
// the flow's verify gate failed on the worktree. verifyOutput is the tail of the
// verify run (stdout+stderr) so the agent sees exactly what failed. The agent
// must keep working until the changes pass verify cleanly — the step is not
// complete until then.
func implementFixPrompt(it *flowsdk.Item, verifyCmd, verifyOutput string) string {
	dc := baseContext(it, verifyCmd)
	dc.VerifyOutput = truncate(strings.TrimSpace(verifyOutput), 2000)
	return render("implement_fix.tmpl", dc)
}

// rebaseConflictPrompt drives the smart conflict-resolution turn of the commit
// step. The flow already committed the work and rebased onto the latest origin,
// and the rebase stopped on merge conflicts (the runner cannot resolve them).
// conflicts is the list of conflicted paths the runner reported; rebaseOutput is
// the raw rebase output. The generic resolve-and-continue guidance comes from the
// shared RebaseResolution partial; this template adds the Promise re-embed note.
func rebaseConflictPrompt(it *flowsdk.Item, verifyCmd string, conflicts []string, rebaseOutput string) string {
	dc := baseContext(it, verifyCmd)
	dc.Conflicts = "(see rebase output)"
	if len(conflicts) > 0 {
		dc.Conflicts = strings.Join(conflicts, ", ")
	}
	dc.RebaseOutput = truncate(strings.TrimSpace(rebaseOutput), 800)
	return render("rebase_conflict.tmpl", dc)
}

func reviewPrompt(it *flowsdk.Item, verifyCmd string) string {
	name := "review_task.tmpl"
	if it.Type == flowsdk.ItemPlan {
		name = "review_plan.tmpl"
	}
	return render(name, baseContext(it, verifyCmd))
}

func coveragePrompt(it *flowsdk.Item, verifyCmd string) string {
	return render("coverage.tmpl", baseContext(it, verifyCmd))
}

// summaryPrompt drives the dedicated post-push resolution-summary turn. It runs
// from the SAME continuing session as the work (the doer holds the full narrative)
// AFTER the change is committed and pushed, so the recap describes the merged
// result rather than a mid-implementation intention. Read-only by contract.
func summaryPrompt(it *flowsdk.Item, verifyCmd string) string {
	return render("summary.tmpl", baseContext(it, verifyCmd))
}

func inspectPrompt(it *flowsdk.Item, verifyCmd string) string {
	return render("inspect.tmpl", baseContext(it, verifyCmd))
}

// truncate clips s to at most n characters, appending an ellipsis. Used to bound
// the verify/rebase output embedded in the implement-fix and rebase-conflict
// prompts.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
