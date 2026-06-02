// Command do is the Promise project's tracker "flow" binary: a stateless,
// per-step workflow executable implementing the task/bug lifecycle as steps —
// plan → implementation → review → coverage → commit → push → summary →
// inspection. It is built on the OSS flow SDK's declarative cli.Run(cli.App{...})
// shape; the tracker-server-backed Backend / Agent / Worktree / Telemetry
// surfaces live in flow-sdk/pkg/backend/tracker.
//
// It is the Promise counterpart of the tracker repo's reference `do` flow: the
// step machine and lease/status plumbing are project-agnostic (owned by the OSS
// cli package), but the per-step agent prompts and the verify gate are
// Promise-specific (see prompts.go and steps.go) — Promise has its own skills,
// build, and test commands (bin/build, bin/verify --wasm, bin/promise test).
//
// CLI (provided by the OSS cli package):
//
//	do doctor              verify backend prereqs
//	do list                list items this flow can process
//	do claim <id>          acquire a claim on an item (alias: do lease <id>)
//	do run-step            advance ONE lifecycle item (one prompt → one artifact)
//	do status [<id>]       read-only lifecycle checklist
//	do grant <key> ...     extend a parked step's budget
//	do release             drop the claim
//
// The runner spawns `do run-step` for ONE step at a time; the next step is
// always derived from durable item state by the OSS orchestrator, never passed
// in.
package main

import (
	"fmt"
	"os"
	"time"

	trackerbackend "djabi.dev/go/flow_sdk/pkg/backend/tracker"
	"github.com/promise-language/flow"
	"github.com/promise-language/flow/cli"

	"github.com/promise-language/promise/flows/internal/srchash"
)

// flowBinaryName is the name the tracker dispatches against. Must match the
// build target (./bin/flow/do) and the item.Flow value the tracker resolves for
// items of this type.
const flowBinaryName = "do"

// verifyCmd is Promise's pre-commit / pre-push gate: format + vet + the full
// host+WASM test suite (CLAUDE.md: "Always run `bin/verify --wasm` before
// committing"). It is the flow's agent-proof line of defense — the
// implement/commit/push steps only proceed once it passes on the worktree.
// Configured ONCE here as cli.App.VerifyCmd; step handlers read it via
// StepCtx.VerifyCmd() to run the gate AND feed it into prompt context (so the
// shared, project-agnostic prompt fragments refer to the same command).
const verifyCmd = "bin/verify --wasm"

// sourceHash is the flow source hash baked in at build time by ./make
// (-ldflags "-X main.sourceHash=..."). It stays "dev" for `go run` / dlv debug
// builds, which skip the staleness check. See srchash.CheckStale.
var sourceHash = "dev"

func main() {
	// Refuse to run a stale binary, exactly like the other bin/ tools: if flow
	// source (flows/, flow/, or flow-sdk/) changed since this binary was built,
	// tell the user to ./make rather than silently running outdated logic. Runs
	// before cli.Run touches os.Args so even `do status` is gated.
	srchash.CheckStale(sourceHash)

	backend, err := trackerbackend.NewBackend(trackerbackend.Config{
		BinaryName: flowBinaryName,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "do: backend init:", err)
		os.Exit(1)
	}
	os.Exit(cli.Run(buildApp(backend)))
}

// buildApp constructs the cli.App for the do binary. Extracted from main so a
// test can build the same App against a Backend pointed at a fake tracker +
// runner. A single function is the single source of truth for the artifact set,
// the flow definitions, and the preflight chain.
func buildApp(backend *trackerbackend.Backend) cli.App {
	artifacts := []flow.ArtifactDef{
		flow.Artifact("plan", flow.ArtifactMarkdown),
		flow.Artifact("phases", flow.ArtifactJSON),
		flow.Artifact("implementation", flow.ArtifactPatch),
		flow.Artifact("review", flow.ArtifactMarkdown),
		flow.Artifact("coverage", flow.ArtifactMarkdown),
		flow.Artifact("commit", flow.ArtifactCommitHash),
		flow.Artifact("push", flow.ArtifactCommitHash),
		flow.Artifact("summary", flow.ArtifactMarkdown),
		flow.Artifact("inspection", flow.ArtifactJSON),
	}

	// do-task: the full code lifecycle for task/bug items. The implement step's
	// MaxPromptsPerInvocation caps the verify-fix loop — the metered Agent refuses
	// the Nth prompt within one invocation, surfacing ErrBudgetExhausted that
	// cli.RunOne translates to a park. The resolution summary is produced by the
	// dedicated post-push summary step (after push, before inspect) so it reflects
	// the merged result rather than a mid-implementation guess.
	doTask := flow.NewFlow("do-task", []flow.ItemType{"task", "bug"})
	doTask.AddStep("create plan", "plan", makeStepPlan(backend), flow.Required)
	doTask.AddStep("implement", "implementation", makeStepImplementation(backend),
		flow.Required,
		flow.MaxPromptsPerInvocation(defaultImplementVerifyMaxRounds),
		flow.Timeout(verifyStepTimeout),
	)
	doTask.AddStep("review and fix issues", "review", makeStepReview(backend), flow.Required)
	doTask.AddStep("fill coverage gaps", "coverage", makeStepCoverage(backend), flow.Required)
	// commit and push both run the full `bin/verify --wasm` gate via the flow, so
	// they take the same generous timeout as implement (see verifyStepTimeout) —
	// the default 30m StepBudget would cut the slow host+WASM suite short.
	doTask.AddStep("commit to local", "commit", makeStepCommit(backend), flow.Required, flow.Timeout(verifyStepTimeout))
	doTask.AddStep("push to origin", "push", makeStepPush(backend), flow.Required, flow.Timeout(verifyStepTimeout))
	doTask.AddStep("summarize resolution", "summary", makeStepSummary(backend), flow.Required)
	doTask.AddStep("inspect", "inspection", makeStepInspection(backend), flow.Required)

	// do-plan: plan items don't write code — they break themselves into child task
	// items and exit. Review still runs (the plan IS the work product) but phases
	// replaces every code-touching step.
	doPlan := flow.NewFlow("do-plan", []flow.ItemType{"plan"})
	doPlan.AddStep("plan", "plan", makeStepPlan(backend), flow.Required)
	doPlan.AddStep("review and fix issues", "review", makeStepReview(backend), flow.Required)
	doPlan.AddStep("file phase tasks", "phases", makeStepPhases(backend), flow.Required)

	return cli.App{
		Name:      flowBinaryName,
		Backend:   backend,
		Agent:     trackerbackend.NewAgent(backend),
		Telemetry: trackerbackend.NewTelemetry(backend),
		Preflight: backend.PreflightAll(),
		Artifacts: artifacts,
		Flows:     []*flow.Flow{doTask, doPlan},
		VerifyCmd: verifyCmd,
	}
}

// verifyStepTimeout bounds the steps that run the full `bin/verify --wasm` gate
// through the flow (implement, commit, push). The host+WASM suite is slow — the
// pre-OSS Promise flow budgeted 45m for a single verify run — so these steps get a
// generous cap, well above the 30m DefaultStepBudget, to keep the OSS step
// deadline from cutting verify short and parking the step. Steps that do not run a
// flow-driven verify keep the default budget.
const verifyStepTimeout = 60 * time.Minute

// defaultImplementVerifyMaxRounds bounds the implement step's verify-fix loop at
// 3 rounds by default. Maps to OSS's MaxPromptsPerInvocation(3): after 3
// Agent.Run calls within a single invocation, the metered agent returns
// ErrBudgetExhausted and cli.RunOne parks the step with an axis=prompts budget
// exhaustion. A follow-up invocation re-enters with a fresh per-invocation
// prompt budget. Operators can pass `do grant implementation --prompts <N>` to
// extend the cap once.
const defaultImplementVerifyMaxRounds = 3
