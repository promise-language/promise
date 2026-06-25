package common

import (
	"errors"
	"maps"
	"strings"
	"testing"
	"time"
)

// release_ci_test.go is the hermetic suite for `bin/release ci` (the standalone
// ci.yml dispatcher). It swaps the git/gh seams for in-memory fakes so no
// `git`/`gh` process is ever spawned.

// ── fakes ───────────────────────────────────────────────────────────────────

type fakeCIGit struct {
	branch    string
	head      string
	remote    map[string]string // branch → origin tip ("" / absent = not on origin)
	branchErr error
	headErr   error
	remoteErr error
}

func (g *fakeCIGit) CurrentBranch() (string, error) { return g.branch, g.branchErr }
func (g *fakeCIGit) HeadSHA() (string, error)       { return g.head, g.headErr }
func (g *fakeCIGit) RemoteBranchSHA(b string) (string, error) {
	if g.remoteErr != nil {
		return "", g.remoteErr
	}
	return g.remote[b], nil
}

// fakeCIGH is an in-memory cutGH. WorkflowRuns flips from runsBefore to runsAfter
// once a dispatch happens, so the --watch path (baseline-then-poll) is exercised
// without a real workflow.
type fakeCIGH struct {
	dispatched     []map[string]string // each: inputs + {workflow, ref}
	dispatchErr    error
	runsBefore     []ghRun
	runsAfter      []ghRun
	runsBeforeErr  error                       // returned by WorkflowRuns before dispatch
	runsAfterErr   error                       // returned by WorkflowRuns after dispatch
	jobs           map[int64][]ghJob
	runJobsFn      func(int64) ([]ghJob, error) // overrides jobs map when set
	dispatchedFlag bool
}

func (f *fakeCIGH) DispatchWorkflow(workflow, ref string, inputs map[string]string) error {
	if f.dispatchErr != nil {
		return f.dispatchErr
	}
	rec := map[string]string{"workflow": workflow, "ref": ref}
	maps.Copy(rec, inputs)
	f.dispatched = append(f.dispatched, rec)
	f.dispatchedFlag = true
	return nil
}

func (f *fakeCIGH) WorkflowRuns(workflow string, limit int) ([]ghRun, error) {
	if f.dispatchedFlag {
		return f.runsAfter, f.runsAfterErr
	}
	return f.runsBefore, f.runsBeforeErr
}

func (f *fakeCIGH) RunJobs(id int64) ([]ghJob, error) {
	if f.runJobsFn != nil {
		return f.runJobsFn(id)
	}
	return f.jobs[id], nil
}

// withCINow replaces the nowFn clock seam for the duration of t.
func withCINow(t *testing.T, fn func() time.Time) {
	t.Helper()
	prev := nowFn
	nowFn = fn
	t.Cleanup(func() { nowFn = prev })
}

// withCITTY overrides the isCIStdoutTTY seam for the duration of t.
func withCITTY(t *testing.T, isTTY bool) {
	t.Helper()
	prev := isCIStdoutTTY
	isCIStdoutTTY = func() bool { return isTTY }
	t.Cleanup(func() { isCIStdoutTTY = prev })
}

// withCIFakes swaps the package-level ci seams for the duration of a test.
func withCIFakes(t *testing.T, git ciGit, gh cutGH) {
	t.Helper()
	prevGit, prevGH := defaultCIGit, defaultCIGH
	defaultCIGit = func(string) ciGit { return git }
	defaultCIGH = gh
	t.Cleanup(func() { defaultCIGit, defaultCIGH = prevGit, prevGH })
}

const ciSHA = "abcdef0123456789abcdef0123456789abcdef01"

// happyCIGit is "on main, HEAD == origin/main tip" — the all-green precondition.
func happyCIGit() *fakeCIGit {
	return &fakeCIGit{branch: "main", head: ciSHA, remote: map[string]string{"main": ciSHA}}
}

// ── platform resolution ──────────────────────────────────────────────────────

func TestResolveCIPlatforms(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
		err  string
	}{
		{name: "default is linux only", in: nil, want: []string{"linux-amd64"}},
		{name: "all", in: []string{"all"}, want: []string{"all"}},
		{name: "linux alias", in: []string{"linux"}, want: []string{"linux-amd64"}},
		{name: "darwin aliases", in: []string{"mac"}, want: []string{"darwin-arm64"}},
		{name: "windows alias", in: []string{"win"}, want: []string{"windows-amd64"}},
		{name: "canonical name", in: []string{"darwin-arm64"}, want: []string{"darwin-arm64"}},
		{name: "case insensitive", in: []string{"Linux", "WINDOWS"}, want: []string{"linux-amd64", "windows-amd64"}},
		{name: "multiple specific", in: []string{"linux", "darwin", "windows"}, want: []string{"linux-amd64", "darwin-arm64", "windows-amd64"}},
		{name: "dedup", in: []string{"linux", "linux-amd64"}, want: []string{"linux-amd64"}},
		{name: "unknown", in: []string{"freebsd"}, err: "unknown platform"},
		{name: "all cannot combine", in: []string{"all", "linux"}, err: "cannot be combined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveCIPlatforms(tc.in)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("want error containing %q, got %v", tc.err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSplitCIArgs(t *testing.T) {
	plats, flags := splitCIArgs([]string{"linux", "--ref", "feature", "darwin", "--no-tests"})
	if strings.Join(plats, ",") != "linux,darwin" {
		t.Errorf("platforms = %v, want [linux darwin]", plats)
	}
	if strings.Join(flags, " ") != "--ref feature --no-tests" {
		t.Errorf("flags = %v", flags)
	}
	// --ref=inline must not swallow the following token.
	plats, flags = splitCIArgs([]string{"--ref=dev", "linux"})
	if strings.Join(plats, ",") != "linux" || strings.Join(flags, " ") != "--ref=dev" {
		t.Errorf("inline ref: platforms=%v flags=%v", plats, flags)
	}
}

// ── dispatch behavior ─────────────────────────────────────────────────────────

func TestReleaseCIDefaultDispatchesLinux(t *testing.T) {
	gh := &fakeCIGH{}
	withCIFakes(t, happyCIGit(), gh)
	if err := runReleaseCI(t.TempDir(), nil); err != nil {
		t.Fatalf("runReleaseCI: %v", err)
	}
	if len(gh.dispatched) != 1 {
		t.Fatalf("want 1 dispatch, got %d", len(gh.dispatched))
	}
	d := gh.dispatched[0]
	if d["workflow"] != "ci.yml" || d["ref"] != "main" || d["platform"] != "linux-amd64" || d["run_tests"] != "true" {
		t.Errorf("unexpected dispatch: %v", d)
	}
}

func TestReleaseCIAllIsSingleRun(t *testing.T) {
	gh := &fakeCIGH{}
	withCIFakes(t, happyCIGit(), gh)
	if err := runReleaseCI(t.TempDir(), []string{"all"}); err != nil {
		t.Fatalf("runReleaseCI: %v", err)
	}
	if len(gh.dispatched) != 1 || gh.dispatched[0]["platform"] != "all" {
		t.Fatalf("want single platform=all dispatch, got %v", gh.dispatched)
	}
}

func TestReleaseCIMultipleSpecificFanOut(t *testing.T) {
	gh := &fakeCIGH{}
	withCIFakes(t, happyCIGit(), gh)
	if err := runReleaseCI(t.TempDir(), []string{"linux", "darwin"}); err != nil {
		t.Fatalf("runReleaseCI: %v", err)
	}
	if len(gh.dispatched) != 2 {
		t.Fatalf("want 2 dispatches, got %d: %v", len(gh.dispatched), gh.dispatched)
	}
	got := []string{gh.dispatched[0]["platform"], gh.dispatched[1]["platform"]}
	if strings.Join(got, ",") != "linux-amd64,darwin-arm64" {
		t.Errorf("fan-out platforms = %v", got)
	}
}

func TestReleaseCINoTests(t *testing.T) {
	gh := &fakeCIGH{}
	withCIFakes(t, happyCIGit(), gh)
	if err := runReleaseCI(t.TempDir(), []string{"--no-tests"}); err != nil {
		t.Fatalf("runReleaseCI: %v", err)
	}
	if gh.dispatched[0]["run_tests"] != "false" {
		t.Errorf("run_tests = %q, want false", gh.dispatched[0]["run_tests"])
	}
}

// ── current-commit guard ─────────────────────────────────────────────────────

func TestReleaseCIRefusesWhenHeadNotRemoteTip(t *testing.T) {
	git := happyCIGit()
	git.head = "ffffffffffffffffffffffffffffffffffffffff" // diverged from origin/main tip
	gh := &fakeCIGH{}
	withCIFakes(t, git, gh)
	err := runReleaseCI(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "not the tip of origin/main") {
		t.Fatalf("want HEAD-not-tip error, got %v", err)
	}
	if len(gh.dispatched) != 0 {
		t.Errorf("must not dispatch when guard fails, got %v", gh.dispatched)
	}
}

func TestReleaseCIForceBypassesGuard(t *testing.T) {
	git := happyCIGit()
	git.head = "ffffffffffffffffffffffffffffffffffffffff"
	gh := &fakeCIGH{}
	withCIFakes(t, git, gh)
	if err := runReleaseCI(t.TempDir(), []string{"--force"}); err != nil {
		t.Fatalf("--force should dispatch despite diverged HEAD: %v", err)
	}
	if len(gh.dispatched) != 1 || gh.dispatched[0]["ref"] != "main" {
		t.Fatalf("want one dispatch on main, got %v", gh.dispatched)
	}
}

func TestReleaseCIForeignRefSkipsHeadCheck(t *testing.T) {
	git := happyCIGit()
	git.head = "ffffffffffffffffffffffffffffffffffffffff" // irrelevant: --ref names another branch
	git.remote["release"] = ciSHA
	gh := &fakeCIGH{}
	withCIFakes(t, git, gh)
	if err := runReleaseCI(t.TempDir(), []string{"--ref", "release"}); err != nil {
		t.Fatalf("--ref should not run the local-HEAD guard: %v", err)
	}
	if len(gh.dispatched) != 1 || gh.dispatched[0]["ref"] != "release" {
		t.Fatalf("want one dispatch on release, got %v", gh.dispatched)
	}
}

func TestReleaseCIBranchNotOnOrigin(t *testing.T) {
	git := &fakeCIGit{branch: "wip", head: ciSHA, remote: map[string]string{}} // wip not pushed
	gh := &fakeCIGH{}
	withCIFakes(t, git, gh)
	err := runReleaseCI(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "not on origin") {
		t.Fatalf("want not-on-origin error, got %v", err)
	}
	if len(gh.dispatched) != 0 {
		t.Errorf("must not dispatch, got %v", gh.dispatched)
	}
}

func TestReleaseCIDetachedHead(t *testing.T) {
	git := &fakeCIGit{branch: "HEAD", head: ciSHA, remote: map[string]string{}}
	gh := &fakeCIGH{}
	withCIFakes(t, git, gh)
	err := runReleaseCI(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "detached HEAD") {
		t.Fatalf("want detached-HEAD error, got %v", err)
	}
}

func TestReleaseCIUnknownPlatformDoesNotTouchGit(t *testing.T) {
	gh := &fakeCIGH{}
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), []string{"plan9"})
	if err == nil || !strings.Contains(err.Error(), "unknown platform") {
		t.Fatalf("want unknown-platform error, got %v", err)
	}
	if len(gh.dispatched) != 0 {
		t.Errorf("must not dispatch on bad platform, got %v", gh.dispatched)
	}
}

func TestReleaseCIDispatchErrorSurfaces(t *testing.T) {
	gh := &fakeCIGH{dispatchErr: errors.New("gh boom")}
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "gh boom") {
		t.Fatalf("want dispatch error surfaced, got %v", err)
	}
}

// ── --watch ──────────────────────────────────────────────────────────────────

func TestReleaseCIWatchGreen(t *testing.T) {
	noOpSleep(t)
	gh := &fakeCIGH{
		runsAfter: []ghRun{{DatabaseID: 1, HeadSHA: ciSHA, HeadBranch: "main"}},
		jobs:      map[int64][]ghJob{1: {{Name: "linux-amd64", Conclusion: "success"}}},
	}
	withCIFakes(t, happyCIGit(), gh)
	if err := runReleaseCI(t.TempDir(), []string{"--watch"}); err != nil {
		t.Fatalf("watch green: %v", err)
	}
}

func TestReleaseCIWatchFailureExitsNonZero(t *testing.T) {
	noOpSleep(t)
	gh := &fakeCIGH{
		runsAfter: []ghRun{{DatabaseID: 1, HeadSHA: ciSHA}},
		jobs:      map[int64][]ghJob{1: {{Name: "linux-amd64", Conclusion: "failure"}}},
	}
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), []string{"--watch"})
	if err == nil || !strings.Contains(err.Error(), "CI failed") {
		t.Fatalf("want CI-failed error, got %v", err)
	}
}

// TestReleaseCIWatchIgnoresStaleRun is the baseline guarantee: an already-green
// run at the same commit must NOT short-circuit the watch — it must follow the
// run this dispatch creates (here, a failure).
func TestReleaseCIWatchIgnoresStaleRun(t *testing.T) {
	noOpSleep(t)
	stale := ghRun{DatabaseID: 5, HeadSHA: ciSHA} // old green run at the same commit
	fresh := ghRun{DatabaseID: 6, HeadSHA: ciSHA} // the run this dispatch creates
	gh := &fakeCIGH{
		runsBefore: []ghRun{stale},
		runsAfter:  []ghRun{fresh, stale},
		jobs: map[int64][]ghJob{
			5: {{Name: "linux-amd64", Conclusion: "success"}},
			6: {{Name: "linux-amd64", Conclusion: "failure"}},
		},
	}
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), []string{"--watch"})
	if err == nil || !strings.Contains(err.Error(), "CI failed") {
		t.Fatalf("watch must follow the fresh run (failure), not the stale green one; got %v", err)
	}
}

func TestReleaseCIWatchAllPlatforms(t *testing.T) {
	noOpSleep(t)
	jobs := make([]ghJob, 0, len(requiredPlatforms))
	for _, p := range requiredPlatforms {
		jobs = append(jobs, ghJob{Name: p, Conclusion: "success"})
	}
	gh := &fakeCIGH{
		runsAfter: []ghRun{{DatabaseID: 1, HeadSHA: ciSHA}},
		jobs:      map[int64][]ghJob{1: jobs},
	}
	withCIFakes(t, happyCIGit(), gh)
	if err := runReleaseCI(t.TempDir(), []string{"all", "--watch"}); err != nil {
		t.Fatalf("watch all green: %v", err)
	}
}

// TestReleaseCIWatchTimeout: clock advances past 3h with no CI result → "timed out".
func TestReleaseCIWatchTimeout(t *testing.T) {
	noOpSleep(t)
	// Each nowFn call jumps past ciWatchTimeout so the deadline check fires immediately.
	tick := time.Now()
	withCINow(t, func() time.Time {
		tick = tick.Add(ciWatchTimeout + time.Second)
		return tick
	})
	gh := &fakeCIGH{
		runsAfter: []ghRun{{DatabaseID: 1, HeadSHA: ciSHA}},
		jobs:      map[int64][]ghJob{1: {}}, // no jobs → linux-amd64 stays absent
	}
	withCITTY(t, false)
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), []string{"--watch"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timed-out error, got %v", err)
	}
}

// TestReleaseCIWatchOutputTTY: TTY path executes without error (in-place \r branch).
func TestReleaseCIWatchOutputTTY(t *testing.T) {
	noOpSleep(t)
	withCITTY(t, true)
	gh := &fakeCIGH{
		runsAfter: []ghRun{{DatabaseID: 1, HeadSHA: ciSHA}},
		jobs:      map[int64][]ghJob{1: {{Name: "linux-amd64", Conclusion: "success"}}},
	}
	withCIFakes(t, happyCIGit(), gh)
	if err := runReleaseCI(t.TempDir(), []string{"--watch"}); err != nil {
		t.Fatalf("TTY watch should succeed: %v", err)
	}
}

// TestReleaseCIWatchOutputNonTTY: non-TTY path executes without error (newline branch).
func TestReleaseCIWatchOutputNonTTY(t *testing.T) {
	noOpSleep(t)
	withCITTY(t, false)
	gh := &fakeCIGH{
		runsAfter: []ghRun{{DatabaseID: 1, HeadSHA: ciSHA}},
		jobs:      map[int64][]ghJob{1: {{Name: "linux-amd64", Conclusion: "success"}}},
	}
	withCIFakes(t, happyCIGit(), gh)
	if err := runReleaseCI(t.TempDir(), []string{"--watch"}); err != nil {
		t.Fatalf("non-TTY watch should succeed: %v", err)
	}
}

// TestReleaseCIWatchBaselineQueryError: WorkflowRuns fails during the pre-dispatch
// baseline capture (latestCIRunID error path).
func TestReleaseCIWatchBaselineQueryError(t *testing.T) {
	gh := &fakeCIGH{runsBeforeErr: errors.New("api down")}
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), []string{"--watch"})
	if err == nil || !strings.Contains(err.Error(), "api down") {
		t.Fatalf("want baseline-query error surfaced, got %v", err)
	}
	if len(gh.dispatched) != 0 {
		t.Errorf("must not dispatch when baseline capture fails, got %v", gh.dispatched)
	}
}

// TestReleaseCIWatchStatusQueryError: WorkflowRuns fails inside the watch loop
// (ciStatusFromNewRuns error path — exercises "query CI status" error in watchCIRuns).
func TestReleaseCIWatchStatusQueryError(t *testing.T) {
	noOpSleep(t)
	withCITTY(t, false)
	gh := &fakeCIGH{
		runsAfterErr: errors.New("gh unavailable"),
	}
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), []string{"--watch"})
	if err == nil || !strings.Contains(err.Error(), "gh unavailable") {
		t.Fatalf("want watch-loop query error surfaced, got %v", err)
	}
}

// TestReleaseCIWatchJobsQueryError: RunJobs fails inside the watch loop
// (the RunJobs error branch in ciStatusFromNewRuns).
func TestReleaseCIWatchJobsQueryError(t *testing.T) {
	noOpSleep(t)
	withCITTY(t, false)
	gh := &fakeCIGH{
		runsAfter: []ghRun{{DatabaseID: 1, HeadSHA: ciSHA}},
		runJobsFn: func(int64) ([]ghJob, error) {
			return nil, errors.New("jobs fetch failed")
		},
	}
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), []string{"--watch"})
	if err == nil || !strings.Contains(err.Error(), "jobs fetch failed") {
		t.Fatalf("want jobs-query error surfaced, got %v", err)
	}
}

// TestReleaseCIWatchTTYTrailingNewline: TTY mode where the first poll returns a
// pending platform (triggering the \r progress write and wroteProgress=true), then
// the second poll returns success — verifying the trailing fmt.Println() executes.
func TestReleaseCIWatchTTYTrailingNewline(t *testing.T) {
	noOpSleep(t)
	withCITTY(t, true)
	var calls int
	gh := &fakeCIGH{
		runsAfter: []ghRun{{DatabaseID: 1, HeadSHA: ciSHA}},
		runJobsFn: func(int64) ([]ghJob, error) {
			calls++
			if calls == 1 {
				return []ghJob{{Name: "linux-amd64"}}, nil // no conclusion → absent
			}
			return []ghJob{{Name: "linux-amd64", Conclusion: "success"}}, nil
		},
	}
	withCIFakes(t, happyCIGit(), gh)
	if err := runReleaseCI(t.TempDir(), []string{"--watch"}); err != nil {
		t.Fatalf("TTY trailing-newline path should succeed: %v", err)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 RunJobs calls (one pending, one green), got %d", calls)
	}
}

// TestReleaseCIWatchErrorAfterTTYProgress: TTY mode where the first poll writes \r
// progress (wroteProgress=true), then the second poll returns an error — verifying
// that watchCIRuns prints a trailing newline before returning the error.
func TestReleaseCIWatchErrorAfterTTYProgress(t *testing.T) {
	noOpSleep(t)
	withCITTY(t, true)
	var calls int
	gh := &fakeCIGH{
		runsAfter: []ghRun{{DatabaseID: 1, HeadSHA: ciSHA}},
		runJobsFn: func(int64) ([]ghJob, error) {
			calls++
			if calls == 1 {
				return []ghJob{{Name: "linux-amd64"}}, nil // absent → triggers \r write
			}
			return nil, errors.New("transient error") // second poll errors
		},
	}
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), []string{"--watch"})
	if err == nil || !strings.Contains(err.Error(), "transient error") {
		t.Fatalf("want transient error surfaced, got %v", err)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 RunJobs calls, got %d", calls)
	}
}

// ── CLI wiring ───────────────────────────────────────────────────────────────

func TestRunReleaseDispatchesCI(t *testing.T) {
	gh := &fakeCIGH{}
	withCIFakes(t, happyCIGit(), gh)
	if err := RunRelease(t.TempDir(), []string{"ci"}); err != nil {
		t.Fatalf("RunRelease ci: %v", err)
	}
	if len(gh.dispatched) != 1 {
		t.Fatalf("RunRelease did not route to runReleaseCI (dispatched %d)", len(gh.dispatched))
	}
}
