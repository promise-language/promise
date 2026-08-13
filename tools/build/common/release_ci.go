package common

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// release_ci.go implements `bin/release ci [platform...]` — the direct trigger for
// the manual-dispatch CI workflow (.github/workflows/ci.yml). It is distinct from
// `cut`'s CI handling, where a dispatch is only a side effect of the green-CI
// release gate; this is the standalone "run CI on the commit I'm on" command.
//
// workflow_dispatch can only target a branch/tag ref — never an arbitrary commit
// SHA — and actions/checkout re-fetches that ref BY NAME when the job starts,
// seconds after the dispatch call returns. So a dispatch has two shapes:
//
//   - no --commit: dispatch on the current branch, after verifying local HEAD is
//     that branch's pushed tip (override with --force) — CI runs the remote tip.
//   - --commit <commit-ish>: resolve it to a concrete SHA, push an immutable
//     `ci-pin-<short>` tag there, and dispatch on the tag. The tag must OUTLIVE
//     the dispatch call (T1489): deleting it right after dispatching made every
//     pinned run fail with `couldn't find remote ref` at checkout.
//
// Pin lifetime: with --watch the tag is deleted once the run finishes; without
// it the tag stays on origin and the next pinned dispatch prunes it (a pin whose
// ci.yml run has completed). ci.yml's `concurrency: {group: ci-${{github.ref}},
// cancel-in-progress: true}` also means two dispatches sharing a ref cancel each
// other, so `ci` refuses to dispatch onto a ref that already has a live run
// (--cancel-running overrides) and refuses a multi-platform fan-out outright
// (`ci all` is the whole matrix in ONE run).

// pinGit is the pin-tag surface shared by `ci --commit` and `cut --commit`. Both
// create the same immutable `ci-pin-<short-sha>` ref, so the create/list/delete
// primitives live in one place and both subcommands reuse pinTagName,
// requirePinnableCommit and prunePinTags.
type pinGit interface {
	PushTagAt(tag, sha string) error              // git push origin sha:refs/tags/tag
	DeletePinTag(tag string) error                // git push origin --delete refs/tags/tag (+ local)
	ListRemotePinTags() ([]string, error)         // git ls-remote --tags origin 'refs/tags/ci-pin-*'
	ReachableFromOrigin(sha string) (bool, error) // sha is on some refs/remotes/origin/*
}

// pinTagName is the ONE naming rule for pin refs. `ci --commit` and `cut --commit`
// must agree on it: they prune each other's pins, and a pin the other side cannot
// recognise would either be collected while live or never collected at all.
func pinTagName(sha string) string { return "ci-pin-" + short(sha) }

// requirePinnableCommit rejects a commit origin does not already have. Two
// reasons, both load-bearing: workflow_dispatch checks the commit out from
// origin, and `git push origin <sha>:refs/tags/<pin>` would otherwise push the
// local-only commit objects to origin as a silent side effect of "run CI".
func requirePinnableCommit(git pinGit, sha string) error {
	reachable, err := git.ReachableFromOrigin(sha)
	if err != nil {
		return fmt.Errorf("reachability check: %w", err)
	}
	if !reachable {
		return fmt.Errorf("commit %s is not on origin — push it first\n"+
			"    (workflow_dispatch checks the commit out from origin, so it must be pushed).", short(sha))
	}
	return nil
}

// ciGit is the minimal git surface `ci` needs (RemoteBranchSHA is not on cutGit —
// only `ci` compares against the pushed tip). The gh surface is the existing
// cutGH (DispatchWorkflow, CancelRun, and the WorkflowRuns/RunJobs pair --watch
// polls). The production shellGit/shellGH satisfy both; release_ci_test.go swaps
// in fakes.
type ciGit interface {
	pinGit
	CurrentBranch() (string, error)
	HeadSHA() (string, error)
	RemoteBranchSHA(branch string) (string, error) // origin tip, "" if absent
	Fetch() error                                  // git fetch origin --tags
	ResolveSHA(ref string) (string, error)         // git rev-parse <ref>^{commit}
}

// defaultCIGit/defaultCIGH are the production seams; tests swap them.
var (
	defaultCIGit       = func(root string) ciGit { return shellGit{root: root} }
	defaultCIGH  cutGH = shellGH{}
)

// ciPlatformAliases maps user-friendly platform tokens to the canonical values
// ci.yml's `platform` choice input accepts. "all" fans the whole matrix out in a
// single run; the OS short names save typing the `-arch` suffix.
//
// Every canonical name in requiredPlatforms must be a key here, or that platform
// cannot be dispatched by name — including by gateCI, which asks for an absent
// platform by its canonical name. Bare "linux" stays amd64: it is the cheap
// default and silently redirecting it to arm64 would surprise. "linux-aarch64"
// is accepted because that is what `uname -m` reports on the machine you are
// most likely typing this from.
var ciPlatformAliases = map[string]string{
	"all":           "all",
	"linux":         "linux-amd64",
	"linux-amd64":   "linux-amd64",
	"linux-arm64":   "linux-arm64",
	"linux-aarch64": "linux-arm64",
	"darwin":        "darwin-arm64",
	"mac":           "darwin-arm64",
	"macos":         "darwin-arm64",
	"darwin-arm64":  "darwin-arm64",
	"windows":       "windows-amd64",
	"win":           "windows-amd64",
	"windows-amd64": "windows-amd64",
}

// runReleaseCI dispatches ci.yml for the requested platform. No platform =
// linux-amd64 only (the cheap default); `all` is the whole matrix in one run.
func runReleaseCI(root string, args []string) error {
	platforms, flags := splitCIArgs(args)
	fs := flag.NewFlagSet("ci", flag.ContinueOnError)
	commit := fs.String("commit", "", "pin CI to this commit-ish (commit, branch or tag — resolved to a concrete SHA)")
	noTests := fs.Bool("no-tests", false, "build only — skip the test suite (cheap toolchain check; macOS bills 10x)")
	force := fs.Bool("force", false, "dispatch even if local HEAD is not the tip of the remote branch")
	watch := fs.Bool("watch", false, "after dispatching, poll until the run(s) finish; exit non-zero if CI is red")
	cancelRunning := fs.Bool("cancel-running", false, "cancel an in-progress ci.yml run on the same ref and dispatch anyway")
	if err := fs.Parse(flags); err != nil {
		return err
	}

	// --force only bypasses the HEAD-is-remote-tip guard, which the pinned path
	// never runs — combining them can only mean the caller misunderstood one.
	if *commit != "" && *force {
		return fmt.Errorf("ci: --commit and --force are mutually exclusive")
	}

	targets, err := resolveCIPlatforms(platforms)
	if err != nil {
		return err
	}
	// Every dispatch here shares one ref, and ci.yml cancels in-progress runs
	// sharing github.ref — so a fan-out would cancel itself run by run.
	if len(targets) > 1 {
		return fmt.Errorf("ci: dispatching %s would create %d ci.yml runs on the same ref\n"+
			"    and each would cancel the previous (ci.yml cancels in-progress runs sharing github.ref).\n"+
			"    Use `bin/release ci all` to cover the whole matrix in ONE run, or dispatch one\n"+
			"    platform at a time.", strings.Join(targets, ", "), len(targets))
	}
	platform := targets[0]

	git := defaultCIGit(root)
	var sha, dispatchRef, pinTag string
	if *commit != "" {
		// Fetch first: both the commit-ish resolution (e.g. `origin/main`) and the
		// reachability check read remote-tracking refs.
		if ferr := git.Fetch(); ferr != nil {
			return fmt.Errorf("ci: git fetch: %w", ferr)
		}
		resolved, rerr := git.ResolveSHA(*commit)
		if rerr != nil {
			return fmt.Errorf("ci: resolve --commit %q: %w", *commit, rerr)
		}
		// Resolve BEFORE deriving the pin name: `short()` of a commit-ish like
		// `HEAD~2` would produce a nonsense tag that pins nothing.
		sha = strings.TrimSpace(resolved)
		if cerr := requirePinnableCommit(git, sha); cerr != nil {
			return fmt.Errorf("ci: %w", cerr)
		}
		pinTag = pinTagName(sha)
		dispatchRef = pinTag
	} else {
		branch, berr := git.CurrentBranch()
		if berr != nil {
			return fmt.Errorf("ci: current branch: %w", berr)
		}
		if branch == "" || branch == "HEAD" {
			return fmt.Errorf("ci: detached HEAD — pass --commit <commit> (workflow_dispatch needs a pushed branch or tag ref; --commit creates a pin tag for you)")
		}
		// workflow_dispatch checks out the branch's REMOTE tip, so resolve it first:
		// an absent branch can never be dispatched, and the tip is what CI will run.
		remote, rerr := git.RemoteBranchSHA(branch)
		if rerr != nil {
			return fmt.Errorf("ci: resolve origin/%s: %w", branch, rerr)
		}
		if remote == "" {
			return fmt.Errorf("ci: ref %q is not on origin — push it first (workflow_dispatch requires a pushed branch or tag)", branch)
		}
		// `ci` means "test the commit I'm on", so guard that local HEAD IS the remote
		// tip CI would check out. --force is an explicit "dispatch on the remote tip
		// regardless" and skips the check.
		if !*force {
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
		sha = remote
		dispatchRef = branch
	}

	runTests := "true"
	if *noTests {
		runTests = "false"
	}
	gh := defaultCIGH

	// ONE run list feeds three consumers: the --watch baseline, the stale-pin
	// prune, and the "would this dispatch cancel a live run?" guard.
	runs, err := gh.WorkflowRuns("ci.yml", ciWatchRunLimit)
	if err != nil {
		return fmt.Errorf("ci: list runs: %w", err)
	}
	// --watch follows the run THIS dispatch creates, not a stale completed run
	// already sitting at the same SHA — so only runs above this ID count.
	baseline := maxRunID(runs)
	if pinTag != "" {
		prunePinTags(git, runs, pinTag)
	}
	if err := guardConcurrentRun(gh, runs, dispatchRef, *cancelRunning); err != nil {
		return err
	}
	if pinTag != "" {
		// Idempotent for this naming scheme: the tag name encodes the SHA, so a
		// re-push lands the same object.
		if perr := git.PushTagAt(pinTag, sha); perr != nil {
			return fmt.Errorf("ci: push pin tag %s: %w", pinTag, perr)
		}
	}

	fmt.Printf("Dispatching ci.yml on %s @ %s (run_tests=%s):\n", dispatchRef, short(sha), runTests)
	if derr := gh.DispatchWorkflow("ci.yml", dispatchRef, map[string]string{"platform": platform, "run_tests": runTests}); derr != nil {
		deletePinTag(git, pinTag)
		return fmt.Errorf("ci: dispatch %s: %w", platform, derr)
	}
	fmt.Printf("  • platform=%s\n", platform)

	if *watch {
		// The pin ref must survive until actions/checkout has fetched it — tie its
		// lifetime to the RUN, not to the dispatch call (T1489). A watch that ends
		// without the run concluding (wall-clock ceiling, or a failed status query)
		// leaves a run that may still be queued, so the pin stays and a later
		// dispatch's prune collects it once its run is genuinely done.
		werr := watchCIRuns(gh, sha, targets, baseline)
		var pending *ciWatchPendingError
		if errors.As(werr, &pending) {
			if pinTag != "" {
				fmt.Fprintf(os.Stderr, "note: leaving pin tag %s on origin — the run has not finished\n", pinTag)
			}
		} else {
			deletePinTag(git, pinTag)
		}
		return werr
	}
	if pinTag != "" {
		fmt.Printf("Pin tag %s stays on origin until the run completes; a later dispatch prunes it.\n", pinTag)
	}
	fmt.Println("Track: gh run list --workflow ci.yml")
	return nil
}

// deletePinTag removes a pin tag best-effort: a leftover pin is collected by the
// next prune, so failing the command over it would be worse than a warning.
func deletePinTag(git pinGit, tag string) {
	if tag == "" {
		return
	}
	if err := git.DeletePinTag(tag); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not delete pin tag %s: %v\n", tag, err)
	}
}

// prunePinTags deletes `ci-pin-*` tags from origin whose dispatched run has
// finished. A pin is removed only on POSITIVE evidence of completion — at least
// one completed ci.yml run on that tag ref and none still queued/in-progress —
// so a pin whose run has not surfaced in the API yet (another invocation
// dispatched seconds ago) is never collected out from under it (T1490 tracks the
// tail case: a run that has scrolled out of the run list can never satisfy this).
// `keep` is the caller's own pin, skipped so a repeat dispatch at the same commit
// does not delete and immediately re-push the identical ref. Failures are
// warnings: a stale pin costs nothing but a ref.
func prunePinTags(git pinGit, runs []ghRun, keep string) {
	tags, err := git.ListRemotePinTags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not list pin tags: %v\n", err)
		return
	}
	for _, tag := range tags {
		if tag == keep {
			continue
		}
		var completed, active bool
		for _, r := range runs {
			if r.HeadBranch != tag {
				continue
			}
			if isRunActive(r.Status) {
				active = true
			} else if r.Status == "completed" {
				completed = true
			}
		}
		if !completed || active {
			continue
		}
		if derr := git.DeletePinTag(tag); derr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not prune pin tag %s: %v\n", tag, derr)
			continue
		}
		fmt.Printf("pruned stale pin tag %s\n", tag)
	}
}

// isRunActive reports whether a ci.yml run is still occupying its concurrency
// group — i.e. a new dispatch on the same ref would cancel it.
func isRunActive(status string) bool {
	switch status {
	case "queued", "in_progress", "requested", "waiting", "pending":
		return true
	}
	return false
}

// activeCIRuns returns the runs a dispatch on dispatchRef would cancel: ci.yml's
// concurrency group is `ci-${{ github.ref }}`, so the ref name IS the group key.
// Two pins at DIFFERENT commits therefore have distinct refs and never collide.
func activeCIRuns(runs []ghRun, dispatchRef string) []ghRun {
	var out []ghRun
	for _, r := range runs {
		if r.HeadBranch == dispatchRef && isRunActive(r.Status) {
			out = append(out, r)
		}
	}
	return out
}

// ciRunActiveError is the "dispatching here would silently cancel a live run"
// refusal. Typed so `cut` can surface it as an overridable gate detail while
// `ci` returns it verbatim.
type ciRunActiveError struct{ msg string }

func (e *ciRunActiveError) Error() string { return e.msg }

// ciCancelHint is `ci`'s way out of the collision refusal. `cut` passes its own
// (it defines no --cancel-running flag, so naming one would send the reader into
// a "flag provided but not defined" dead end).
const ciCancelHint = "Wait for it to finish, or pass --cancel-running to cancel it and dispatch anyway."

// concurrentRunError names the run that would be cancelled and, in `hint`, the
// caller's own way out — every caller must supply an escape hatch IT actually
// offers.
func concurrentRunError(gh cutGH, run ghRun, dispatchRef, hint string) error {
	return &ciRunActiveError{msg: fmt.Sprintf(
		"an in-progress ci.yml run (#%d%s) is active on `%s`;\n"+
			"    dispatching now would cancel it (ci.yml cancels in-progress runs sharing github.ref).\n"+
			"    %s",
		run.DatabaseID, runPlatformClause(gh, run.DatabaseID), dispatchRef, hint)}
}

// runPlatformClause is the ", platform=…" context in the collision error. Jobs
// are a nicety, so a jobs-query failure yields an empty clause — it must never
// turn the guard itself into an error.
func runPlatformClause(gh cutGH, runID int64) string {
	jobs, err := gh.RunJobs(runID)
	if err != nil {
		return ""
	}
	var plats []string
	seen := map[string]bool{}
	for _, j := range jobs {
		p := jobPlatform(j.Name)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		plats = append(plats, p)
	}
	if len(plats) == 0 {
		return ""
	}
	return ", platform=" + strings.Join(plats, "+")
}

// guardConcurrentRun refuses a dispatch that would cancel a live run on the same
// ref, unless cancelRunning explicitly opts into cancelling it.
func guardConcurrentRun(gh cutGH, runs []ghRun, dispatchRef string, cancelRunning bool) error {
	for _, r := range activeCIRuns(runs, dispatchRef) {
		if !cancelRunning {
			return fmt.Errorf("ci: %w", concurrentRunError(gh, r, dispatchRef, ciCancelHint))
		}
		if err := gh.CancelRun(r.DatabaseID); err != nil {
			return fmt.Errorf("ci: cancel in-progress run #%d: %w", r.DatabaseID, err)
		}
		fmt.Printf("ci: cancelled in-progress run #%d on %s\n", r.DatabaseID, dispatchRef)
	}
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

// maxRunID is the highest run database ID in a run list (0 if empty). Captured
// pre-dispatch as the --watch baseline so the watch follows only runs created
// afterward.
func maxRunID(runs []ghRun) int64 {
	var maxID int64
	for _, r := range runs {
		if r.DatabaseID > maxID {
			maxID = r.DatabaseID
		}
	}
	return maxID
}

// ciWatchTimeout is the wall-clock ceiling for ci --watch. Kept separate from
// ciPollAttempts (used by cut's watchCI) so the two ceilings can differ.
const ciWatchTimeout = 3 * time.Hour

// isCIStdoutTTY reports whether stdout is an interactive terminal — used to
// choose between in-place \r progress and newline-per-poll fallback.
var isCIStdoutTTY = func() bool {
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// ciNonTTYLogEvery throttles newline-per-poll output when stdout is not a TTY.
const ciNonTTYLogEvery = 3

// ciWatchPendingError marks a watch that ended WITHOUT the run concluding — the
// wall-clock ceiling was hit, or the status query itself failed. The run may
// still be queued, so its pin ref must be left alone; deleting a ref a pending
// job is about to check out is exactly the T1489 failure.
type ciWatchPendingError struct{ err error }

func (e *ciWatchPendingError) Error() string { return e.err.Error() }
func (e *ciWatchPendingError) Unwrap() error { return e.err }

// watchCIRuns polls until every wanted platform's job — in a run created after
// `baseline` at `sha` — has finished, then reports. It returns an error if any
// platform is red (a plain error: the run concluded) or the 3h ceiling is
// exceeded (a *ciWatchPendingError: it did not). On a TTY, progress is updated
// in-place with \r; otherwise newlines are printed every ciNonTTYLogEvery polls.
func watchCIRuns(gh cutGH, sha string, targets []string, baseline int64) error {
	want := expandCITargets(targets)
	start := nowFn()
	deadline := start.Add(ciWatchTimeout)
	tty := isCIStdoutTTY()

	var status map[string]ciConclusion
	poll := 0
	wroteProgress := false
	for nowFn().Before(deadline) {
		s, err := ciStatusFromNewRuns(gh, sha, baseline, want)
		if err != nil {
			if wroteProgress {
				fmt.Println()
			}
			return &ciWatchPendingError{fmt.Errorf("ci: query CI status: %w", err)}
		}
		status = s
		pending := platformsAt(want, status, ciAbsent)
		if len(pending) == 0 {
			break
		}
		elapsed := nowFn().Sub(start).Truncate(time.Second)
		msg := fmt.Sprintf("  [%s] waiting on %s...", elapsed, strings.Join(pending, ", "))
		if tty {
			fmt.Printf("\r%-80s", msg)
			wroteProgress = true
		} else if poll%ciNonTTYLogEvery == 0 {
			fmt.Println(msg)
		}
		poll++
		sleepFn(ciPollInterval)
	}

	if wroteProgress {
		fmt.Println()
	}

	if pending := platformsAt(want, status, ciAbsent); len(pending) != 0 {
		return &ciWatchPendingError{fmt.Errorf("ci: timed out waiting for CI; still pending: %s", strings.Join(pending, ", "))}
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
// Only --commit takes a value (--no-tests/--force/--watch/--cancel-running are
// booleans); Go's flag package stops at the first positional, so positionals and
// flags can't be interleaved through it directly — hence this pre-split mirrors
// splitPositionalFlags but only treats --commit as value-taking.
func splitCIArgs(args []string) (platforms, flags []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if name := strings.TrimLeft(a, "-"); name == "commit" && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		platforms = append(platforms, a)
	}
	return platforms, flags
}
