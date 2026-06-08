// Command do is the Promise project's tracker "flow" binary: the per-step
// task/bug lifecycle — plan → implement → review → coverage → land
// (commit+push) → summary → inspection.
//
// The step machine, push-lease / smart-rebase logic, artifact set, flow
// definitions, and the push-lease/e2e test suite are SHARED with the tracker
// project via the importable flow-sdk/doflow package. This binary is a thin
// shim: it supplies the Promise-specific seam — the verify command
// (bin/verify --wasm), the implement / land step budgets, and the agent prompt
// builders (see prompts.go) — as a doflow.Config and calls doflow.BuildApp.
// The Promise prompt bodies stay here because their text references
// Promise-specific build/test mechanics; the domain-agnostic prompt skeleton
// comes from the shared flow/prompt package (used by prompts.go).
//
// CLI (provided by the OSS cli package):
//
//	do doctor              verify backend prereqs
//	do list                list items this flow can process
//	do claim <id>          acquire a claim on an item (alias: do lease <id>)
//	do run-step            advance ONE lifecycle item (one prompt → one artifact)
//	do resolve [<id>]      run ALL steps until finalized or parked
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

	"djabi.dev/go/flow_sdk/doflow"
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
// committing"). It is the flow's agent-proof line of defense — the implement and
// land steps only proceed once it passes on the worktree. Configured ONCE here
// (doflow.Config.VerifyCmd → cli.App.VerifyCmd); step handlers read it via
// StepCtx.VerifyCmd() to run the gate AND feed it into the prompt builders, so
// the bodies and the shared prompt fragments refer to the same command.
const verifyCmd = "bin/verify --wasm"

// formatCmd is the formatter the LAND step runs to normalize the worktree
// BEFORE committing (CLAUDE.md: bin/format formats Go + Promise — the SAME
// files bin/verify formats). Running it first makes the to-be-committed tree
// canonical, so the pre-push verify can never strand a format diff in the
// worktree after the commit (T0767). Threaded via doflow.Config.FormatCmd into
// stepCommitPush; unlike verifyCmd it is not surfaced to any prompt.
const formatCmd = "bin/format"

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
	os.Exit(cli.Run(doflow.BuildApp(promiseConfig(), backend)))
}

// promiseConfig is the per-project seam handed to doflow.BuildApp: the verify
// command, the per-step budgets, and the Promise-specific agent prompt builders
// (prompts.go). CommitMessage is left nil — doflow's DefaultCommitMessage
// ("<ID>: <Title>" + a model-tagged Co-Authored-By trailer) is exactly
// Promise's convention.
//
// The budgets are a balancing act between two competing objectives on the
// autonomous production line: parking more than ~3 items stalls production (a
// park needs operator attention), which we want to avoid — so budgets must be
// generous enough that a healthy step finishes within them rather than tripping
// a premature park. At the same time the caps must be tight enough to prevent
// runaway resource waste when a step genuinely misbehaves.
//
// DefaultStepBudget sets the baseline every step inherits per-axis: 30m, 3
// invocations, 2 prompts/invocation, $10. The 2 prompts/invocation is the
// load-bearing default — it gives the implement and land steps their one
// verify-fix retry (re-prompt the agent with the verify failure); at the flow
// SDK's own default of 1 they could never fix a failed gate. Per-step overrides
// raise only the axes where a step is heavier than the baseline (unset axes
// keep the default): plan can be complex and expensive (1h, $20); implementation
// runs the slow `bin/verify --wasm` gate and the largest changes (3h, $30);
// review and land each get 1h (land sometimes does a complex smart rebase). The
// remaining steps (phases, coverage, summary, inspection) run on the baseline.
func promiseConfig() doflow.Config {
	return doflow.Config{
		FlowBinaryName: flowBinaryName,
		VerifyCmd:      verifyCmd,
		FormatCmd:      formatCmd,
		DefaultStepBudget: flow.StepBudget{
			Timeout:                 30 * time.Minute,
			MaxInvocations:          3,
			MaxPromptsPerInvocation: 2,
			MaxCostUSD:              10,
		},
		PlanBudget: flow.StepBudget{
			Timeout:    1 * time.Hour,
			MaxCostUSD: 20, // planning can sometimes be quite complex and expensive
		},
		ImplementationBudget: flow.StepBudget{
			Timeout:    3 * time.Hour,
			MaxCostUSD: 30, // allow for larger complex task execution
		},
		ReviewBudget: flow.StepBudget{
			Timeout: 1 * time.Hour,
		},
		LandBudget: flow.StepBudget{
			Timeout: 1 * time.Hour, // land sometimes needs to do a complex smart rebase
		},
		Prompts: doflow.Prompts{
			Plan:           planPrompt,
			Phases:         phasesPrompt,
			Implement:      implementPrompt,
			ImplementFix:   implementFixPrompt,
			Review:         reviewPrompt,
			Coverage:       coveragePrompt,
			RebaseConflict: rebaseConflictPrompt,
			Summary:        summaryPrompt,
			Inspect:        inspectPrompt,
		},
	}
}
