package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	flowsdk "djabi.dev/go/flow_sdk"
	"djabi.dev/go/flow_sdk/runner"
)

// cmdLease pins an item to a worktree for manual (hand-driven) flow work. Unlike
// `do run`, lease MUST take the item id positionally: it is the bootstrap that
// CREATES the context file, so there is nothing to read the id from yet. It posts
// to the runner's loopback /v1/lease — the runner writes
// <worktree>/.flow/context.json (taking the lease) and marks the item manual on
// the tracker. Afterwards `do run` / `do release` work from that file with no
// arguments. Connectivity (runner URL, tracker token, worktree) comes from env
// (RUNNER_API_URL / TRACKER_AUTH_TOKEN / FLOW_WORKTREE) since no file exists yet.
func cmdLease(args []string) int {
	var idArg string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			idArg = a
			break
		}
	}
	if idArg == "" {
		fmt.Fprintln(os.Stderr, "do lease: an item id is required (usage: do lease <id>)")
		return 2
	}
	c, err := flowsdk.LoadContext()
	if err != nil {
		fmt.Fprintln(os.Stderr, "do lease:", err)
		return 1
	}
	if c.RunnerURL == "" {
		fmt.Fprintln(os.Stderr, "do lease: missing runner_url (set "+flowsdk.EnvRunnerURL+" to the runner's loopback URL)")
		return 1
	}
	res, err := runner.Lease(context.Background(), c.RunnerURL, c.TrackerToken, flowsdk.LeaseRequest{
		ItemID:   flowsdk.ItemID(idArg),
		Flow:     flowName,
		Worktree: c.Worktree,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "do lease:", err)
		return 1
	}
	fmt.Printf("leased %s in %s (invocation %s)\nrun `do run` there to execute one step; `do release` to free it\n",
		idArg, res.Worktree, res.InvocationID)
	return 0
}

// cmdRelease ends a manual lease: it reads the context file written by `do lease`
// (authoritative for the invocation + nonce) and posts to the runner's
// /v1/run/{inv}/release, which removes the context file and clears the tracker
// manual flag + lease so the item is eligible for auto-dispatch again.
func cmdRelease(_ []string) int {
	c, err := flowsdk.LoadContext()
	if err != nil {
		fmt.Fprintln(os.Stderr, "do release:", err)
		return 1
	}
	if err := c.ValidateRunner(); err != nil {
		fmt.Fprintln(os.Stderr, "do release:", err, "— run inside a leased worktree (.flow/context.json)")
		return 1
	}
	// One-shot lease reconcile (see ensureLiveLease): if the runner moved, release
	// the fresh invocation on the current runner rather than the dead old one.
	c, err = ensureLiveLease(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "do release:", err)
		return 1
	}
	res, err := runner.New(c).Release(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "do release:", err)
		return 1
	}
	fmt.Printf("released %s\n", res.Released)
	return 0
}

// ensureLiveLease reconciles the lease against the runner that is CURRENTLY running,
// once, at the start of a command (never in a loop). A runner restart — e.g. an
// auto-update on deploy — gives the runner a new loopback port and wipes its
// in-memory invocation registry, so the stored runner_url/runner_token/invocation_id
// go stale and every runner call fails with "connection refused" (old port) or an
// unknown invocation. The durable lease fields (item, worktree, flow, tracker) survive.
//
// The check is a pure comparison of the runner published in .runner-flow.json
// (DiscoverRunnerInfo) against the runner_url stored in .flow/context.json: if they
// differ the runner moved, so we re-lease the same item ONCE to acquire a fresh
// url+token+invocation. The runner persists the refreshed context file itself; we also
// return the refreshed context so the in-flight command uses it immediately. No-op
// (returns c unchanged) when no runner-info is discoverable or the lease already points
// at the current runner.
func ensureLiveLease(c flowsdk.Context) (flowsdk.Context, error) {
	start := c.Worktree
	if start == "" {
		start = "."
	}
	info, ok := flowsdk.DiscoverRunnerInfo(start)
	if !ok || info.RunnerURL == "" || info.RunnerURL == c.RunnerURL {
		return c, nil // no runner-info, or the lease already points at the current runner
	}
	fmt.Fprintf(os.Stderr, "do: runner moved (%s → %s) — re-leasing %s once\n",
		c.RunnerURL, info.RunnerURL, c.ItemID)
	res, err := runner.Lease(context.Background(), info.RunnerURL, c.TrackerToken, flowsdk.LeaseRequest{
		ItemID:   c.ItemID,
		Flow:     c.FlowName,
		Worktree: c.Worktree,
	})
	if err != nil {
		return c, fmt.Errorf("re-lease against current runner %s: %w", info.RunnerURL, err)
	}
	c.RunnerURL = res.RunnerURL
	c.RunnerToken = res.RunnerToken
	c.InvocationID = res.InvocationID
	if res.Worktree != "" {
		c.Worktree = res.Worktree
	}
	return c, nil
}
