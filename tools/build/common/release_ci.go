package common

import (
	"flag"
	"fmt"
	"strings"
)

// release_ci.go implements `bin/release ci [platform...]` — the direct trigger for
// the manual-dispatch CI workflow (.github/workflows/ci.yml). It is distinct from
// `cut`'s CI handling, where a dispatch is only a side effect of the green-CI
// release gate; this is the standalone "run CI on the commit I'm on" command.
//
// workflow_dispatch can only target a branch/tag ref — never an arbitrary commit
// SHA — and actions/checkout runs at that ref's REMOTE tip. So `ci` dispatches on
// the current branch and verifies the local HEAD is that branch's pushed tip,
// guaranteeing CI tests the current commit (override with --force, or pick a
// branch with --ref).

// ciGit is the minimal git surface `ci` needs (RemoteBranchSHA is not on cutGit —
// only `ci` compares against the pushed tip). The gh surface is the existing
// cutGH (DispatchWorkflow + the WorkflowRuns/RunJobs pair --watch polls). The
// production shellGit/shellGH satisfy both; release_ci_test.go swaps in fakes.
type ciGit interface {
	CurrentBranch() (string, error)
	HeadSHA() (string, error)
	RemoteBranchSHA(branch string) (string, error) // origin tip, "" if absent
}

// defaultCIGit/defaultCIGH are the production seams; tests swap them.
var (
	defaultCIGit       = func(root string) ciGit { return shellGit{root: root} }
	defaultCIGH  cutGH = shellGH{}
)

// ciPlatformAliases maps user-friendly platform tokens to the canonical values
// ci.yml's `platform` choice input accepts. "all" fans the whole matrix out in a
// single run; the OS short names save typing the `-arch` suffix.
var ciPlatformAliases = map[string]string{
	"all":           "all",
	"linux":         "linux-amd64",
	"linux-amd64":   "linux-amd64",
	"darwin":        "darwin-arm64",
	"mac":           "darwin-arm64",
	"macos":         "darwin-arm64",
	"darwin-arm64":  "darwin-arm64",
	"windows":       "windows-amd64",
	"win":           "windows-amd64",
	"windows-amd64": "windows-amd64",
}

// runReleaseCI dispatches ci.yml on the current branch for the requested
// platform(s). No platform = linux-amd64 only (the cheap default).
func runReleaseCI(root string, args []string) error {
	platforms, flags := splitCIArgs(args)
	fs := flag.NewFlagSet("ci", flag.ContinueOnError)
	ref := fs.String("ref", "", "branch to dispatch CI on (default: current branch)")
	noTests := fs.Bool("no-tests", false, "build only — skip the test suite (cheap toolchain check; macOS bills 10x)")
	force := fs.Bool("force", false, "dispatch even if local HEAD is not the tip of the remote branch")
	watch := fs.Bool("watch", false, "after dispatching, poll until the run(s) finish; exit non-zero if CI is red")
	if err := fs.Parse(flags); err != nil {
		return err
	}

	targets, err := resolveCIPlatforms(platforms)
	if err != nil {
		return err
	}

	git := defaultCIGit(root)
	branch := *ref
	if branch == "" {
		b, berr := git.CurrentBranch()
		if berr != nil {
			return fmt.Errorf("ci: current branch: %w", berr)
		}
		branch = b
	}
	if branch == "" || branch == "HEAD" {
		return fmt.Errorf("ci: detached HEAD — pass --ref <branch> (workflow_dispatch needs a branch, not a commit)")
	}

	// workflow_dispatch checks out the branch's REMOTE tip, so resolve it first:
	// an absent branch can never be dispatched, and the tip is what CI will run.
	remote, err := git.RemoteBranchSHA(branch)
	if err != nil {
		return fmt.Errorf("ci: resolve origin/%s: %w", branch, err)
	}
	if remote == "" {
		return fmt.Errorf("ci: branch %q is not on origin — push it first (workflow_dispatch needs a pushed branch)", branch)
	}

	// With no explicit --ref, `ci` means "test the commit I'm on", so guard that
	// local HEAD IS the remote tip CI would check out. --force or a foreign --ref
	// is an explicit "dispatch on the remote tip regardless" and skips the check.
	if *ref == "" && !*force {
		local, lerr := git.HeadSHA()
		if lerr != nil {
			return fmt.Errorf("ci: head sha: %w", lerr)
		}
		if !sameSHA(local, remote) {
			return fmt.Errorf("ci: local HEAD %s is not the tip of origin/%s (%s)\n"+
				"  CI dispatches on the branch ref and runs on its remote tip, not your local commit.\n"+
				"  push first so CI tests this commit — or pass --force to dispatch on the remote tip anyway.",
				short(local), branch, short(remote))
		}
	}

	runTests := "true"
	if *noTests {
		runTests = "false"
	}
	gh := defaultCIGH

	// --watch follows the run THIS dispatch creates, not a stale completed run
	// already sitting at the same SHA — so snapshot the highest run ID first and
	// only consider runs created after it.
	var baseline int64
	if *watch {
		b, lerr := latestCIRunID(gh)
		if lerr != nil {
			return fmt.Errorf("ci: list runs: %w", lerr)
		}
		baseline = b
	}

	fmt.Printf("Dispatching ci.yml on %s @ %s (run_tests=%s):\n", branch, short(remote), runTests)
	for _, p := range targets {
		if derr := gh.DispatchWorkflow("ci.yml", branch, map[string]string{"platform": p, "run_tests": runTests}); derr != nil {
			return fmt.Errorf("ci: dispatch %s: %w", p, derr)
		}
		fmt.Printf("  • platform=%s\n", p)
	}

	if *watch {
		return watchCIRuns(gh, remote, targets, baseline)
	}
	fmt.Println("Track: gh run list --workflow ci.yml")
	return nil
}

// resolveCIPlatforms maps the positional platform tokens to canonical ci.yml
// values. No tokens → the cheap default (linux-amd64 only). "all" must stand
// alone — it already covers the whole matrix, so combining it with specific
// targets is a contradiction worth rejecting rather than silently collapsing.
func resolveCIPlatforms(tokens []string) ([]string, error) {
	if len(tokens) == 0 {
		return []string{"linux-amd64"}, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range tokens {
		canon, ok := ciPlatformAliases[strings.ToLower(t)]
		if !ok {
			return nil, fmt.Errorf("ci: unknown platform %q (want: all, linux, darwin, windows — or a canonical <os>-<arch>)", t)
		}
		if seen[canon] {
			continue
		}
		seen[canon] = true
		out = append(out, canon)
	}
	if seen["all"] {
		if len(out) > 1 {
			return nil, fmt.Errorf("ci: `all` cannot be combined with specific platforms")
		}
		return []string{"all"}, nil
	}
	return out, nil
}

// latestCIRunID is the highest ci.yml run database ID currently known (0 if
// none). Captured pre-dispatch as the --watch baseline so the watch follows only
// runs created afterward.
func latestCIRunID(gh cutGH) (int64, error) {
	runs, err := gh.WorkflowRuns("ci.yml", ciWatchRunLimit)
	if err != nil {
		return 0, err
	}
	var maxID int64
	for _, r := range runs {
		if r.DatabaseID > maxID {
			maxID = r.DatabaseID
		}
	}
	return maxID, nil
}

// watchCIRuns polls until every wanted platform's job — in a run created after
// `baseline` at `sha` — has finished, then reports. It returns an error if any
// platform is red or the wait times out, so `ci --watch` exits non-zero on red CI
// (usable as a script gate). Reuses the cut watch loop's tunables (ciPollInterval/
// ciPollAttempts/sleepFn).
func watchCIRuns(gh cutGH, sha string, targets []string, baseline int64) error {
	want := expandCITargets(targets)
	var status map[string]ciConclusion
	for range ciPollAttempts {
		s, err := ciStatusFromNewRuns(gh, sha, baseline, want)
		if err != nil {
			return fmt.Errorf("ci: query CI status: %w", err)
		}
		status = s
		if pending := platformsAt(want, status, ciAbsent); len(pending) != 0 {
			fmt.Printf("  waiting on %s...\n", strings.Join(pending, ", "))
			sleepFn(ciPollInterval)
			continue
		}
		break
	}
	if pending := platformsAt(want, status, ciAbsent); len(pending) != 0 {
		return fmt.Errorf("ci: timed out waiting for CI; still pending: %s", strings.Join(pending, ", "))
	}
	if failed := platformsAt(want, status, ciFailed); len(failed) != 0 {
		return fmt.Errorf("ci: CI failed for: %s", strings.Join(failed, ", "))
	}
	fmt.Printf("CI green for %s @ %s\n", strings.Join(want, ", "), short(sha))
	return nil
}

// expandCITargets resolves the dispatched targets into the concrete platform/job
// names to wait for — "all" fans out to the full matrix.
func expandCITargets(targets []string) []string {
	if len(targets) == 1 && targets[0] == "all" {
		return append([]string(nil), requiredPlatforms...)
	}
	return targets
}

// platformsAt returns the wanted platforms whose status equals `at`, preserving
// the requested order.
func platformsAt(want []string, status map[string]ciConclusion, at ciConclusion) []string {
	var out []string
	for _, p := range want {
		if status[p] == at {
			out = append(out, p)
		}
	}
	return out
}

// ciStatusFromNewRuns is ciStatusAtSHA restricted to runs created after `baseline`
// and to the `want` platform set — so the watch ignores a stale run sitting at the
// same SHA and reports only the dispatch in flight.
func ciStatusFromNewRuns(gh cutGH, sha string, baseline int64, want []string) (map[string]ciConclusion, error) {
	runs, err := gh.WorkflowRuns("ci.yml", ciWatchRunLimit)
	if err != nil {
		return nil, err
	}
	status := make(map[string]ciConclusion, len(want))
	for _, p := range want {
		status[p] = ciAbsent
	}
	for _, run := range runs {
		if run.DatabaseID <= baseline || !sameSHA(run.HeadSHA, sha) {
			continue
		}
		jobs, err := gh.RunJobs(run.DatabaseID)
		if err != nil {
			return nil, err
		}
		for _, job := range jobs {
			p := jobPlatform(job.Name)
			cur, ok := status[p]
			if !ok || cur != ciAbsent {
				continue // not a wanted platform, or already decided by a newer run
			}
			switch job.Conclusion {
			case "success":
				status[p] = ciGreen
			case "failure", "cancelled", "timed_out", "startup_failure":
				status[p] = ciFailed
			}
		}
	}
	return status, nil
}

// ciWatchRunLimit bounds the `gh run list` page the watch scans — generous enough
// to cover a fan-out plus any concurrent unrelated runs, matching ciStatusAtSHA.
const ciWatchRunLimit = 50

// splitCIArgs partitions `ci` args into platform positionals and flag tokens.
// Only --ref takes a value (--no-tests/--force are booleans); Go's flag package
// stops at the first positional, so positionals and flags can't be interleaved
// through it directly — hence this pre-split mirrors splitPositionalFlags but
// only treats --ref as value-taking.
func splitCIArgs(args []string) (platforms, flags []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if name := strings.TrimLeft(a, "-"); name == "ref" && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		platforms = append(platforms, a)
	}
	return platforms, flags
}
