// Command do is the Promise project's tracker "flow" binary: a single stateless,
// per-step workflow executable implementing the task/bug lifecycle as steps —
// plan → implementation → review → coverage → commit → push → inspection. The
// runner builds it via ./make and spawns `do run` for ONE step at a time; the
// next step is always derived from durable item state, never passed in.
//
// It is the Promise counterpart of the tracker's reference `do` flow: the step
// machine and lease/status plumbing are identical (they are project-agnostic),
// but the per-step agent prompts and the verify gate are Promise-specific (see
// prompts.go and steps.go) — Promise has its own skills, build, and test
// commands.
//
// CLI:
//
//	do lease <id>     pin an item to this worktree for manual work: the runner
//	                  writes .flow/context.json (the lease) and marks the item
//	                  manual on the tracker. The bootstrap that CREATES the
//	                  context file — hence the only mutating command that takes a
//	                  positional id.
//	do run            execute exactly one step on the leased item (the context
//	                  file's item is authoritative — no positional id), print an
//	                  InvocationResult JSON line, exit non-zero iff the step failed.
//	do release        free the lease: the runner removes .flow/context.json and
//	                  clears the tracker manual flag, re-enabling auto-dispatch.
//	do status [<id>]  print the flow's step plan for an item (the one next step
//	                  marked), derived purely from durable item state. Read-only;
//	                  safe to run by hand. --json/--text force the output form.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	flowsdk "djabi.dev/go/flow_sdk"
	"djabi.dev/go/flow_sdk/runner"
	"djabi.dev/go/flow_sdk/tracker"

	"github.com/p5e-ia/promise-lang/flows/internal/srchash"
)

// sourceHash is the flow source hash baked in at build time by ./make
// (-ldflags "-X main.sourceHash=..."). It stays "dev" for `go run` / dlv debug
// builds, which skip the staleness check. See srchash.CheckStale.
var sourceHash = "dev"

func main() {
	// Refuse to run a stale binary, exactly like the other bin/ tools: if flow
	// source (flows/ or flow-sdk/) changed since this binary was built, tell the
	// user to ./make rather than silently running outdated logic.
	srchash.CheckStale(sourceHash)

	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: do <lease|run|release|status> [args]")
		os.Exit(2)
	}
	switch args[0] {
	case "lease":
		os.Exit(cmdLease(args[1:]))
	case "run":
		os.Exit(cmdRun())
	case "release":
		os.Exit(cmdRelease(args[1:]))
	case "status":
		os.Exit(cmdStatus(args[1:]))
	default:
		fmt.Fprintf(os.Stderr, "do: unknown command %q (want lease|run|release|status)\n", args[0])
		os.Exit(2)
	}
}

// cmdRun loads the lease, runs exactly one step, emits the InvocationResult, and
// returns the process exit code (non-zero iff the step failed). It takes NO id —
// the context file's item is authoritative, so a step can never land against the
// wrong item/worktree.
func cmdRun() int {
	c, err := flowsdk.LoadContext()
	if err != nil {
		fmt.Fprintln(os.Stderr, "do run:", err)
		return 1
	}
	if err := c.ValidateTracker(); err != nil {
		fmt.Fprintln(os.Stderr, "do run:", err, "— run inside a leased worktree (.flow/context.json)")
		return 1
	}
	if err := c.ValidateRunner(); err != nil {
		fmt.Fprintln(os.Stderr, "do run:", err)
		return 1
	}
	// One-shot lease reconcile at start: if the runner moved (restart/auto-update),
	// the stored runner_url/token/invocation are stale — re-lease ONCE against the
	// current runner before running the step. No retry loop.
	c, err = ensureLiveLease(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "do run:", err)
		return 1
	}
	agent := c.Agent
	if agent == "" {
		agent = defaultAgent
	}
	f := &flow{
		ctx:      context.Background(),
		tr:       tracker.New(c),
		rn:       runner.New(c),
		id:       c.ItemID,
		inv:      c.InvocationID,
		agent:    agent,
		worktree: c.Worktree,
	}
	res := f.run()
	if err := res.Emit(); err != nil {
		fmt.Fprintln(os.Stderr, "do run: emit result:", err)
		return 1
	}
	return res.ExitCode()
}

// cmdStatus prints the read-only step plan for an item. It needs only tracker
// access; the optional positional id selects any item to inspect (else the leased
// item). It never mutates the item, the worktree, or runs an agent.
func cmdStatus(args []string) int {
	c, err := flowsdk.LoadContext()
	if err != nil {
		fmt.Fprintln(os.Stderr, "do status:", err)
		return 1
	}
	var idArg string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			idArg = a
			break
		}
	}
	id := c.InspectItemID(idArg)
	if c.TrackerURL == "" || id == "" {
		fmt.Fprintln(os.Stderr, "do status: need a tracker URL and an item id (set TRACKER_URL / FLOW_ITEM_ID or pass an id)")
		return 1
	}
	it, err := tracker.New(c).GetItem(context.Background(), id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "do status:", err)
		return 1
	}
	s := buildFlowStatus(it)
	if statusRenderMode(args, isStdoutTTY()) {
		if err := renderStatusJSON(os.Stdout, s); err != nil {
			fmt.Fprintln(os.Stderr, "do status:", err)
			return 1
		}
	} else {
		renderStatusText(os.Stdout, s)
	}
	return 0
}

// isStdoutTTY reports whether stdout is a terminal (character device), so status
// defaults to the human checklist on a TTY and canonical JSON when piped.
func isStdoutTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
