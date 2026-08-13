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
	branch        string
	head          string
	remote        map[string]string // branch → origin tip ("" / absent = not on origin)
	branchErr     error
	headErr       error
	remoteErr     error
	fetchErr      error
	resolved      map[string]string // commit-ish → concrete sha (ResolveSHA)
	resolveErr    error
	reachable     map[string]bool // sha → reachable from some origin ref
	reachableErr  error
	remotePinTags []string // tags ListRemotePinTags reports
	listPinErr    error
	pushedTags    []string // tags pushed via PushTagAt
	deletedTags   []string // tags deleted via DeletePinTag
	pushTagErr    error
	deleteTagErr  error
}

func (g *fakeCIGit) CurrentBranch() (string, error) { return g.branch, g.branchErr }
func (g *fakeCIGit) HeadSHA() (string, error)       { return g.head, g.headErr }
func (g *fakeCIGit) RemoteBranchSHA(b string) (string, error) {
	if g.remoteErr != nil {
		return "", g.remoteErr
	}
	return g.remote[b], nil
}
func (g *fakeCIGit) Fetch() error { return g.fetchErr }
func (g *fakeCIGit) ResolveSHA(ref string) (string, error) {
	if g.resolveErr != nil {
		return "", g.resolveErr
	}
	if sha, ok := g.resolved[ref]; ok {
		return sha, nil
	}
	return ref, nil // unmapped refs resolve to themselves (e.g. a literal sha)
}
func (g *fakeCIGit) ReachableFromOrigin(sha string) (bool, error) {
	if g.reachableErr != nil {
		return false, g.reachableErr
	}
	return g.reachable[sha], nil
}
func (g *fakeCIGit) ListRemotePinTags() ([]string, error) {
	return g.remotePinTags, g.listPinErr
}
func (g *fakeCIGit) PushTagAt(tag, _ string) error {
	if g.pushTagErr != nil {
		return g.pushTagErr
	}
	g.pushedTags = append(g.pushedTags, tag)
	return nil
}
func (g *fakeCIGit) DeletePinTag(tag string) error {
	if g.deleteTagErr != nil {
		return g.deleteTagErr
	}
	g.deletedTags = append(g.deletedTags, tag)
	return nil
}

// fakeCIGH is an in-memory cutGH. WorkflowRuns flips from runsBefore to runsAfter
// once a dispatch happens, so the --watch path (baseline-then-poll) is exercised
// without a real workflow.
type fakeCIGH struct {
	dispatched     []map[string]string // each: inputs + {workflow, ref}
	dispatchErr    error
	runsBefore     []ghRun
	runsAfter      []ghRun
	runsBeforeErr  error // returned by WorkflowRuns before dispatch
	runsAfterErr   error // returned by WorkflowRuns after dispatch
	jobs           map[int64][]ghJob
	runJobsFn      func(int64) ([]ghJob, error) // overrides jobs map when set
	dispatchedFlag bool
	cancelled      []int64 // run IDs passed to CancelRun
	cancelErr      error
}

func (f *fakeCIGH) CancelRun(id int64) error {
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancelled = append(f.cancelled, id)
	return nil
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

// TestEveryRequiredPlatformIsDispatchable ties the two platform lists together.
// They are declared independently — requiredPlatforms in release_cut.go, the
// alias table here — so adding a platform to the release gate without adding it
// here yields a gate that blocks on a CI job no one can dispatch by name, and
// gateCI's own "dispatch the absent platform" path fails on its own request.
func TestEveryRequiredPlatformIsDispatchable(t *testing.T) {
	for _, p := range requiredPlatforms {
		got, err := resolveCIPlatforms([]string{p})
		if err != nil {
			t.Errorf("required platform %q is not dispatchable: %v", p, err)
			continue
		}
		if len(got) != 1 || got[0] != p {
			t.Errorf("required platform %q resolved to %v, want [%s]", p, got, p)
		}
	}
}

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
		{name: "linux-arm64 canonical", in: []string{"linux-arm64"}, want: []string{"linux-arm64"}},
		{name: "linux-aarch64 alias", in: []string{"linux-aarch64"}, want: []string{"linux-arm64"}},
		{name: "bare linux stays amd64", in: []string{"linux", "linux-arm64"}, want: []string{"linux-amd64", "linux-arm64"}},
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
	plats, flags := splitCIArgs([]string{"linux", "--commit", "feature", "darwin", "--no-tests"})
	if strings.Join(plats, ",") != "linux,darwin" {
		t.Errorf("platforms = %v, want [linux darwin]", plats)
	}
	if strings.Join(flags, " ") != "--commit feature --no-tests" {
		t.Errorf("flags = %v", flags)
	}
	// Boolean flags must never swallow the following positional.
	plats, flags = splitCIArgs([]string{"--watch", "linux", "--cancel-running"})
	if strings.Join(plats, ",") != "linux" || strings.Join(flags, " ") != "--watch --cancel-running" {
		t.Errorf("boolean flags: platforms=%v flags=%v", plats, flags)
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

// TestReleaseCIRejectsMultiplePlatforms: naming several platforms would create
// one run per platform on ONE ref, and ci.yml's cancel-in-progress concurrency
// would make each cancel the previous — so the fan-out is refused outright.
func TestReleaseCIRejectsMultiplePlatforms(t *testing.T) {
	gh := &fakeCIGH{}
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), []string{"linux", "darwin"})
	if err == nil {
		t.Fatal("want a multi-platform refusal, got nil")
	}
	for _, want := range []string{"bin/release ci all", "cancel the previous", "linux-amd64", "darwin-arm64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err, want)
		}
	}
	if len(gh.dispatched) != 0 {
		t.Errorf("must not dispatch a self-cancelling fan-out, got %v", gh.dispatched)
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
	// The message must point at the flag that actually solves it.
	if !strings.Contains(err.Error(), "--commit") {
		t.Errorf("detached-HEAD error must name --commit, got %q", err)
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

// ── --commit ──────────────────────────────────────────────────────────────────

const pinnedSHA = "1111111111111111111111111111111111111111"

// pinnedCIGit is happyCIGit plus "pinnedSHA is pushed to origin" — the all-green
// precondition for the --commit path.
func pinnedCIGit() *fakeCIGit {
	g := happyCIGit()
	g.reachable = map[string]bool{pinnedSHA: true}
	return g
}

func pinTagFor(sha string) string { return "ci-pin-" + short(sha) }

// TestSplitCIArgsCommit: --commit is the only value-taking flag.
func TestSplitCIArgsCommit(t *testing.T) {
	plats, flags := splitCIArgs([]string{"linux", "--commit", "abc123", "--no-tests"})
	if strings.Join(plats, ",") != "linux" {
		t.Errorf("platforms = %v, want [linux]", plats)
	}
	if strings.Join(flags, " ") != "--commit abc123 --no-tests" {
		t.Errorf("flags = %v, want [--commit abc123 --no-tests]", flags)
	}
	// inline --commit=abc must not swallow the following token.
	plats, flags = splitCIArgs([]string{"--commit=deadbeef", "linux"})
	if strings.Join(plats, ",") != "linux" || strings.Join(flags, " ") != "--commit=deadbeef" {
		t.Errorf("inline commit: platforms=%v flags=%v", plats, flags)
	}
}

// TestReleaseCICommitDispatchesOnPinTag: the dispatched ref is the pin tag, not the branch.
func TestReleaseCICommitDispatchesOnPinTag(t *testing.T) {
	gh := &fakeCIGH{}
	git := pinnedCIGit()
	withCIFakes(t, git, gh)
	if err := runReleaseCI(t.TempDir(), []string{"linux", "--commit", pinnedSHA}); err != nil {
		t.Fatalf("runReleaseCI: %v", err)
	}
	if len(gh.dispatched) != 1 {
		t.Fatalf("want 1 dispatch, got %d", len(gh.dispatched))
	}
	if got, want := gh.dispatched[0]["ref"], pinTagFor(pinnedSHA); got != want {
		t.Errorf("dispatch ref = %q, want %q (never the branch)", got, want)
	}
	if len(git.pushedTags) != 1 || git.pushedTags[0] != pinTagFor(pinnedSHA) {
		t.Errorf("pushed tags = %v, want [%s]", git.pushedTags, pinTagFor(pinnedSHA))
	}
}

// TestReleaseCICommitPinTagSurvivesDispatch is the T1489 regression test:
// actions/checkout re-fetches the pin ref BY NAME when the job starts, seconds
// after the dispatch call returns, so deleting the tag inside the dispatch made
// every pinned run fail with `couldn't find remote ref`. Without --watch nothing
// tracks the run, so the tag must simply stay.
func TestReleaseCICommitPinTagSurvivesDispatch(t *testing.T) {
	git := pinnedCIGit()
	gh := &fakeCIGH{}
	withCIFakes(t, git, gh)
	if err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA}); err != nil {
		t.Fatalf("runReleaseCI: %v", err)
	}
	if len(git.deletedTags) != 0 {
		t.Fatalf("pin tag must outlive the dispatch call (checkout fetches it by name later); deleted %v", git.deletedTags)
	}
}

// TestReleaseCICommitPinDeletedAfterWatch: with --watch the tag's lifetime is
// tied to the run — deleted exactly once when the watch ends, green or red.
func TestReleaseCICommitPinDeletedAfterWatch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		conclusion string
		wantErr    bool
	}{
		{"green", "success", false},
		{"red", "failure", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noOpSleep(t)
			git := pinnedCIGit()
			gh := &fakeCIGH{
				runsAfter: []ghRun{{DatabaseID: 1, HeadSHA: pinnedSHA, HeadBranch: pinTagFor(pinnedSHA), Status: "completed"}},
				jobs:      map[int64][]ghJob{1: {{Name: "linux-amd64", Conclusion: tc.conclusion}}},
			}
			withCIFakes(t, git, gh)
			err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA, "--watch"})
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			want := pinTagFor(pinnedSHA)
			if len(git.deletedTags) != 1 || git.deletedTags[0] != want {
				t.Errorf("deleted tags = %v, want exactly [%s] after the watch", git.deletedTags, want)
			}
		})
	}
}

// TestReleaseCICommitPinSurvivesUnfinishedWatch: the pin may only be removed once
// the run has CONCLUDED. A watch that gives up (3h ceiling) or dies on a status
// query leaves a run that can still be queued, and deleting the ref it is about
// to check out is the very T1489 failure — so the pin stays for a later prune.
func TestReleaseCICommitPinSurvivesUnfinishedWatch(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		noOpSleep(t)
		tick := time.Now()
		withCINow(t, func() time.Time {
			tick = tick.Add(ciWatchTimeout + time.Second)
			return tick
		})
		withCITTY(t, false)
		git := pinnedCIGit()
		gh := &fakeCIGH{
			runsAfter: []ghRun{{DatabaseID: 1, HeadSHA: pinnedSHA, HeadBranch: pinTagFor(pinnedSHA), Status: "queued"}},
			jobs:      map[int64][]ghJob{1: {}}, // no jobs yet → still pending
		}
		withCIFakes(t, git, gh)
		err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA, "--watch"})
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("want timed-out error, got %v", err)
		}
		if len(git.deletedTags) != 0 {
			t.Errorf("pin must outlive a run that never finished, deleted %v", git.deletedTags)
		}
	})
	t.Run("status query fails", func(t *testing.T) {
		noOpSleep(t)
		git := pinnedCIGit()
		gh := &fakeCIGH{runsAfterErr: errors.New("api down")}
		withCIFakes(t, git, gh)
		err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA, "--watch"})
		if err == nil || !strings.Contains(err.Error(), "api down") {
			t.Fatalf("want the query error surfaced, got %v", err)
		}
		if len(git.deletedTags) != 0 {
			t.Errorf("run state is unknown — pin must stay, deleted %v", git.deletedTags)
		}
	})
}

// TestCIWatchPendingErrorUnwraps: the pending marker is a WRAPPER, not a
// replacement. runReleaseCI matches it with errors.As to decide the pin's fate,
// so it must not swallow the cause — a caller (or a human reading the message)
// still has to see why the watch ended.
func TestCIWatchPendingErrorUnwraps(t *testing.T) {
	noOpSleep(t)
	cause := errors.New("api down")
	git := pinnedCIGit()
	gh := &fakeCIGH{runsAfterErr: cause}
	withCIFakes(t, git, gh)
	err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA, "--watch"})
	if err == nil {
		t.Fatal("want the watch failure surfaced, got nil")
	}
	var pending *ciWatchPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("err %v must be a *ciWatchPendingError so the pin is kept", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("err %v must unwrap to the underlying cause", err)
	}
	if pending.Unwrap() == nil {
		t.Error("Unwrap must expose the wrapped error")
	}
}

// TestReleaseCICommitIshResolved: --commit accepts any commit-ish. The pin name
// must come from the RESOLVED sha — `short("HEAD~2")` would name a tag that pins
// nothing.
func TestReleaseCICommitIshResolved(t *testing.T) {
	for _, ish := range []string{"HEAD~2", "release-branch"} {
		t.Run(ish, func(t *testing.T) {
			git := happyCIGit()
			git.resolved = map[string]string{ish: pinnedSHA}
			git.reachable = map[string]bool{pinnedSHA: true}
			gh := &fakeCIGH{}
			withCIFakes(t, git, gh)
			if err := runReleaseCI(t.TempDir(), []string{"--commit", ish}); err != nil {
				t.Fatalf("runReleaseCI: %v", err)
			}
			want := pinTagFor(pinnedSHA)
			if len(git.pushedTags) != 1 || git.pushedTags[0] != want {
				t.Fatalf("pushed tags = %v, want [%s] (never ci-pin-%s)", git.pushedTags, want, short(ish))
			}
			if gh.dispatched[0]["ref"] != want {
				t.Errorf("dispatch ref = %q, want %q", gh.dispatched[0]["ref"], want)
			}
		})
	}
}

// TestReleaseCICommitNotOnOrigin: workflow_dispatch checks the commit out from
// origin, so an unpushed commit is refused before anything is created.
func TestReleaseCICommitNotOnOrigin(t *testing.T) {
	git := happyCIGit()
	git.reachable = map[string]bool{} // pinnedSHA not on origin
	gh := &fakeCIGH{}
	withCIFakes(t, git, gh)
	err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA})
	if err == nil || !strings.Contains(err.Error(), "is not on origin") {
		t.Fatalf("want not-on-origin error, got %v", err)
	}
	if len(git.pushedTags) != 0 || len(gh.dispatched) != 0 {
		t.Errorf("must not push a pin or dispatch: tags=%v dispatched=%v", git.pushedTags, gh.dispatched)
	}
}

// TestReleaseCICommitResolveError / ...ReachabilityError / ...FetchError: each
// git failure on the pinned path aborts before any mutation.
func TestReleaseCICommitGitErrors(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*fakeCIGit)
		want string
	}{
		{"fetch", func(g *fakeCIGit) { g.fetchErr = errors.New("offline") }, "offline"},
		{"resolve", func(g *fakeCIGit) { g.resolveErr = errors.New("bad revision") }, "bad revision"},
		{"reachability", func(g *fakeCIGit) { g.reachableErr = errors.New("for-each-ref failed") }, "for-each-ref failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			git := pinnedCIGit()
			tc.mut(git)
			gh := &fakeCIGH{}
			withCIFakes(t, git, gh)
			err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q surfaced, got %v", tc.want, err)
			}
			if len(git.pushedTags) != 0 || len(gh.dispatched) != 0 {
				t.Errorf("must not mutate: tags=%v dispatched=%v", git.pushedTags, gh.dispatched)
			}
		})
	}
}

// TestReleaseCICommitWatch: --watch matches against the pinned commit, not the branch tip.
func TestReleaseCICommitWatch(t *testing.T) {
	noOpSleep(t)
	gh := &fakeCIGH{
		runsAfter: []ghRun{{DatabaseID: 1, HeadSHA: pinnedSHA, Status: "completed"}},
		jobs:      map[int64][]ghJob{1: {{Name: "linux-amd64", Conclusion: "success"}}},
	}
	withCIFakes(t, pinnedCIGit(), gh)
	if err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA, "--watch"}); err != nil {
		t.Fatalf("runReleaseCI with --watch: %v", err)
	}
}

// TestReleaseCICommitAndForceExclusive: --commit and --force are mutually exclusive.
func TestReleaseCICommitAndForceExclusive(t *testing.T) {
	gh := &fakeCIGH{}
	withCIFakes(t, pinnedCIGit(), gh)
	err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA, "--force"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutually-exclusive error, got %v", err)
	}
	if len(gh.dispatched) != 0 {
		t.Errorf("must not dispatch when flags are mutually exclusive")
	}
}

// TestReleaseCICommitPushTagError: PushTagAt failure is surfaced and no dispatch happens.
func TestReleaseCICommitPushTagError(t *testing.T) {
	git := pinnedCIGit()
	git.pushTagErr = errors.New("push failed")
	gh := &fakeCIGH{}
	withCIFakes(t, git, gh)
	err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA})
	if err == nil || !strings.Contains(err.Error(), "push failed") {
		t.Fatalf("want push error surfaced, got %v", err)
	}
	if len(gh.dispatched) != 0 {
		t.Errorf("must not dispatch when pin tag push fails")
	}
}

// TestReleaseCICommitDeleteTagWarning: a DeletePinTag failure during cleanup is a
// warning, not an error — a leftover pin is collected by the next prune.
func TestReleaseCICommitDeleteTagWarning(t *testing.T) {
	noOpSleep(t)
	git := pinnedCIGit()
	git.deleteTagErr = errors.New("delete failed")
	gh := &fakeCIGH{
		runsAfter: []ghRun{{DatabaseID: 1, HeadSHA: pinnedSHA, Status: "completed"}},
		jobs:      map[int64][]ghJob{1: {{Name: "linux-amd64", Conclusion: "success"}}},
	}
	withCIFakes(t, git, gh)
	if err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA, "--watch"}); err != nil {
		t.Fatalf("delete-tag failure should only warn, not fail: %v", err)
	}
}

// ── pin-tag pruning ──────────────────────────────────────────────────────────

// TestReleaseCIPrunesFinishedPinTags: a pin whose run completed is collected; a
// pin with a live run, or one with no run in the API yet (another invocation
// dispatched it seconds ago), must be left alone.
func TestReleaseCIPrunesFinishedPinTags(t *testing.T) {
	git := pinnedCIGit()
	git.remotePinTags = []string{"ci-pin-done", "ci-pin-live", "ci-pin-unseen"}
	gh := &fakeCIGH{runsBefore: []ghRun{
		{DatabaseID: 10, HeadBranch: "ci-pin-done", Status: "completed"},
		{DatabaseID: 11, HeadBranch: "ci-pin-live", Status: "in_progress"},
		// ci-pin-live also has an older completed run — the live one still wins.
		{DatabaseID: 9, HeadBranch: "ci-pin-live", Status: "completed"},
	}}
	withCIFakes(t, git, gh)
	if err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA}); err != nil {
		t.Fatalf("runReleaseCI: %v", err)
	}
	if strings.Join(git.deletedTags, ",") != "ci-pin-done" {
		t.Errorf("pruned = %v, want only [ci-pin-done]", git.deletedTags)
	}
}

// TestReleaseCIPruneKeepsOwnPin: a repeat dispatch at the SAME commit finds its
// own pin already on origin with its previous (completed) run. Pruning it would
// delete the ref and immediately re-push the identical one — a window in which a
// job about to check the pin out sees `couldn't find remote ref`, i.e. T1489
// re-opened through the garbage collector. The `keep` skip must hold.
func TestReleaseCIPruneKeepsOwnPin(t *testing.T) {
	own := pinTagFor(pinnedSHA)
	git := pinnedCIGit()
	git.remotePinTags = []string{own, "ci-pin-other"}
	gh := &fakeCIGH{runsBefore: []ghRun{
		{DatabaseID: 10, HeadBranch: own, Status: "completed"},
		{DatabaseID: 11, HeadBranch: "ci-pin-other", Status: "completed"},
	}}
	withCIFakes(t, git, gh)
	if err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA}); err != nil {
		t.Fatalf("runReleaseCI: %v", err)
	}
	if strings.Join(git.deletedTags, ",") != "ci-pin-other" {
		t.Errorf("deleted = %v, want only [ci-pin-other]; the caller's own pin must never be pruned", git.deletedTags)
	}
	// It is still (idempotently) re-pushed, so the ref is guaranteed present.
	if len(git.pushedTags) != 1 || git.pushedTags[0] != own {
		t.Errorf("pushed tags = %v, want [%s]", git.pushedTags, own)
	}
}

// TestReleaseCIPruneOnlyOnPinnedPath: an unpinned dispatch never touches pins —
// it has no pin of its own, and pruning is the pinned path's housekeeping.
func TestReleaseCIPruneOnlyOnPinnedPath(t *testing.T) {
	git := happyCIGit()
	git.remotePinTags = []string{"ci-pin-done"}
	gh := &fakeCIGH{runsBefore: []ghRun{{DatabaseID: 10, HeadBranch: "ci-pin-done", Status: "completed"}}}
	withCIFakes(t, git, gh)
	if err := runReleaseCI(t.TempDir(), nil); err != nil {
		t.Fatalf("runReleaseCI: %v", err)
	}
	if len(git.deletedTags) != 0 {
		t.Errorf("branch dispatch must not prune pins, deleted %v", git.deletedTags)
	}
}

// TestReleaseCIPinLifecycleClosesWithoutWatch is the resource invariant for the
// whole pin mechanism: a pin ref is a mutable resource created on ORIGIN, and the
// un-watched path deliberately leaks it (the run still needs it — T1489). The
// promise that makes that safe is "a later dispatch prunes it", so exercise both
// invocations in sequence: pin #1 must survive its own dispatch and then be
// collected by the next pinned dispatch, once its run has completed.
func TestReleaseCIPinLifecycleClosesWithoutWatch(t *testing.T) {
	firstSHA, secondSHA := pinnedSHA, "2222222222222222222222222222222222222222"
	firstPin, secondPin := pinTagFor(firstSHA), pinTagFor(secondSHA)

	// Invocation 1: dispatch at firstSHA without --watch.
	git := happyCIGit()
	git.reachable = map[string]bool{firstSHA: true, secondSHA: true}
	gh := &fakeCIGH{}
	withCIFakes(t, git, gh)
	if err := runReleaseCI(t.TempDir(), []string{"--commit", firstSHA}); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	if strings.Join(git.pushedTags, ",") != firstPin || len(git.deletedTags) != 0 {
		t.Fatalf("after dispatch 1: pushed=%v deleted=%v, want the pin pushed and kept", git.pushedTags, git.deletedTags)
	}

	// Invocation 2, later: origin still carries pin #1, whose run has since
	// completed. A dispatch at a DIFFERENT commit must collect it.
	git.pushedTags, git.deletedTags = nil, nil
	git.remotePinTags = []string{firstPin}
	gh = &fakeCIGH{runsBefore: []ghRun{{DatabaseID: 10, HeadBranch: firstPin, Status: "completed"}}}
	withCIFakes(t, git, gh)
	if err := runReleaseCI(t.TempDir(), []string{"--commit", secondSHA}); err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if strings.Join(git.deletedTags, ",") != firstPin {
		t.Errorf("deleted = %v, want [%s]; an un-watched pin must not outlive the next pinned dispatch", git.deletedTags, firstPin)
	}
	if strings.Join(git.pushedTags, ",") != secondPin {
		t.Errorf("pushed = %v, want [%s]", git.pushedTags, secondPin)
	}
	// Distinct pins are distinct concurrency groups, so run #1 (if it were still
	// live) would never have been cancelled by this dispatch either.
	if len(gh.dispatched) != 1 || gh.dispatched[0]["ref"] != secondPin {
		t.Errorf("dispatched = %v, want one run on %s", gh.dispatched, secondPin)
	}
}

// TestReleaseCIPruneErrorsAreWarnings: neither a list failure nor a delete
// failure may abort the dispatch.
func TestReleaseCIPruneErrorsAreWarnings(t *testing.T) {
	t.Run("list fails", func(t *testing.T) {
		git := pinnedCIGit()
		git.listPinErr = errors.New("ls-remote failed")
		gh := &fakeCIGH{}
		withCIFakes(t, git, gh)
		if err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA}); err != nil {
			t.Fatalf("a pin-list failure must only warn: %v", err)
		}
		if len(gh.dispatched) != 1 {
			t.Errorf("want the dispatch to proceed, got %v", gh.dispatched)
		}
	})
	t.Run("delete fails", func(t *testing.T) {
		git := pinnedCIGit()
		git.remotePinTags = []string{"ci-pin-done"}
		git.deleteTagErr = errors.New("delete failed")
		gh := &fakeCIGH{runsBefore: []ghRun{{DatabaseID: 10, HeadBranch: "ci-pin-done", Status: "completed"}}}
		withCIFakes(t, git, gh)
		if err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA}); err != nil {
			t.Fatalf("a prune failure must only warn: %v", err)
		}
	})
}

// ── in-progress collision guard ───────────────────────────────────────────────

// TestReleaseCIRejectsWhenRunInProgress covers the three collision cases the
// concurrency group implies: same branch (any platform) collides, the same pin
// ref collides, and a DIFFERENT pin ref does not.
func TestReleaseCIRejectsWhenRunInProgress(t *testing.T) {
	otherSHA := "2222222222222222222222222222222222222222"
	cases := []struct {
		name       string
		args       []string
		runs       []ghRun
		wantReject bool
	}{
		{
			name:       "same branch, other platform",
			runs:       []ghRun{{DatabaseID: 31660226927, HeadBranch: "main", Status: "in_progress"}},
			wantReject: true,
		},
		{
			name:       "same branch, queued",
			runs:       []ghRun{{DatabaseID: 31660226927, HeadBranch: "main", Status: "queued"}},
			wantReject: true,
		},
		{
			name:       "same pin ref",
			args:       []string{"--commit", pinnedSHA},
			runs:       []ghRun{{DatabaseID: 31660226927, HeadBranch: pinTagFor(pinnedSHA), Status: "in_progress"}},
			wantReject: true,
		},
		{
			name: "different pin ref",
			args: []string{"--commit", pinnedSHA},
			runs: []ghRun{{DatabaseID: 31660226927, HeadBranch: pinTagFor(otherSHA), Status: "in_progress"}},
		},
		{
			name: "completed run on same ref",
			runs: []ghRun{{DatabaseID: 31660226927, HeadBranch: "main", Status: "completed"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			git := pinnedCIGit()
			gh := &fakeCIGH{
				runsBefore: tc.runs,
				jobs:       map[int64][]ghJob{31660226927: {{Name: "linux-amd64"}}},
			}
			withCIFakes(t, git, gh)
			err := runReleaseCI(t.TempDir(), tc.args)
			if !tc.wantReject {
				if err != nil {
					t.Fatalf("must not reject a non-colliding run: %v", err)
				}
				if len(gh.dispatched) != 1 {
					t.Errorf("want the dispatch to proceed, got %v", gh.dispatched)
				}
				return
			}
			if err == nil {
				t.Fatal("want a collision refusal, got nil")
			}
			// The message must name the override flag, the run, and the ref.
			for _, want := range []string{"--cancel-running", "#31660226927", "platform=linux-amd64", "cancel it"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q must mention %q", err, want)
				}
			}
			if len(gh.dispatched) != 0 {
				t.Errorf("must not dispatch over a live run, got %v", gh.dispatched)
			}
			if len(git.pushedTags) != 0 {
				t.Errorf("must not push a pin when the guard refuses, got %v", git.pushedTags)
			}
		})
	}
}

// TestReleaseCICollisionErrorWithoutJobs: a jobs-query failure must degrade the
// platform clause, never turn the guard itself into a different error.
func TestReleaseCICollisionErrorWithoutJobs(t *testing.T) {
	gh := &fakeCIGH{
		runsBefore: []ghRun{{DatabaseID: 42, HeadBranch: "main", Status: "in_progress"}},
		runJobsFn:  func(int64) ([]ghJob, error) { return nil, errors.New("jobs unavailable") },
	}
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "--cancel-running") {
		t.Fatalf("want the collision refusal, got %v", err)
	}
	if strings.Contains(err.Error(), "platform=") {
		t.Errorf("platform clause must be omitted when jobs are unavailable: %q", err)
	}
}

// TestReleaseCICollisionPlatformClause: the clause names each platform ONCE and
// ignores jobs that map to no platform (ci.yml's setup/summary jobs). A raw join
// of every job name would render "platform=linux-amd64+linux-amd64" and leak
// non-platform job names into the refusal.
func TestReleaseCICollisionPlatformClause(t *testing.T) {
	gh := &fakeCIGH{
		runsBefore: []ghRun{{DatabaseID: 42, HeadBranch: "main", Status: "in_progress"}},
		jobs: map[int64][]ghJob{42: {
			{Name: "setup"},                     // maps to no platform → skipped
			{Name: "linux-amd64"},               //
			{Name: "linux-amd64 (retry)"},       // same platform again → deduped
			{Name: "build darwin-arm64 (test)"}, // substring match on a decorated name
		}},
	}
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), nil)
	if err == nil {
		t.Fatal("want the collision refusal, got nil")
	}
	if !strings.Contains(err.Error(), "platform=linux-amd64+darwin-arm64") {
		t.Errorf("error %q must carry a deduped, ordered platform clause", err)
	}
	if strings.Contains(err.Error(), "setup") {
		t.Errorf("non-platform job names must not leak into the clause: %q", err)
	}
}

// TestReleaseCICollisionClauseAllNonPlatformJobs: every job maps to no platform
// (a run that has only reached its setup job), so the clause degrades to empty
// rather than rendering a bare ", platform=".
func TestReleaseCICollisionClauseAllNonPlatformJobs(t *testing.T) {
	gh := &fakeCIGH{
		runsBefore: []ghRun{{DatabaseID: 42, HeadBranch: "main", Status: "in_progress"}},
		jobs:       map[int64][]ghJob{42: {{Name: "setup"}, {Name: "summary"}}},
	}
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "--cancel-running") {
		t.Fatalf("want the collision refusal, got %v", err)
	}
	if strings.Contains(err.Error(), "platform=") {
		t.Errorf("clause must be omitted when no job maps to a platform: %q", err)
	}
}

// TestReleaseCICancelRunningDispatches: --cancel-running cancels the live run(s)
// then proceeds.
func TestReleaseCICancelRunningDispatches(t *testing.T) {
	gh := &fakeCIGH{runsBefore: []ghRun{
		{DatabaseID: 77, HeadBranch: "main", Status: "in_progress"},
		{DatabaseID: 78, HeadBranch: "main", Status: "queued"},
		{DatabaseID: 79, HeadBranch: "other", Status: "in_progress"}, // different ref → untouched
	}}
	withCIFakes(t, happyCIGit(), gh)
	if err := runReleaseCI(t.TempDir(), []string{"--cancel-running"}); err != nil {
		t.Fatalf("--cancel-running should dispatch: %v", err)
	}
	if len(gh.cancelled) != 2 || gh.cancelled[0] != 77 || gh.cancelled[1] != 78 {
		t.Errorf("cancelled = %v, want [77 78]", gh.cancelled)
	}
	if len(gh.dispatched) != 1 {
		t.Errorf("want 1 dispatch after cancelling, got %v", gh.dispatched)
	}
}

// TestReleaseCICancelRunError: a failing `gh run cancel` aborts before dispatch —
// dispatching anyway is exactly the silent cancellation the guard exists to stop.
func TestReleaseCICancelRunError(t *testing.T) {
	gh := &fakeCIGH{
		runsBefore: []ghRun{{DatabaseID: 77, HeadBranch: "main", Status: "in_progress"}},
		cancelErr:  errors.New("cancel refused"),
	}
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), []string{"--cancel-running"})
	if err == nil || !strings.Contains(err.Error(), "cancel refused") {
		t.Fatalf("want cancel error surfaced, got %v", err)
	}
	if len(gh.dispatched) != 0 {
		t.Errorf("must not dispatch when the cancel failed, got %v", gh.dispatched)
	}
}

// ── retired flag names ────────────────────────────────────────────────────────

// TestRetiredCommitFlagsError: --commit-hash / --sha / --ref are gone. They must
// ERROR, never be silently accepted or ignored, so a stale script fails loudly.
func TestRetiredCommitFlagsError(t *testing.T) {
	cases := []struct {
		sub  string
		args []string
	}{
		{"ci", []string{"ci", "--ref", "main"}},
		{"ci", []string{"ci", "--commit-hash", "abc123"}},
		{"ci", []string{"ci", "--sha", "abc123"}},
		{"cut", []string{"cut", "next", "--sha", "abc123"}},
		{"cut", []string{"cut", "next", "--commit-hash", "abc123"}},
		{"changes", []string{"changes", "--commit-hash", "abc123"}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			gh := &fakeCIGH{}
			withCIFakes(t, happyCIGit(), gh)
			var err error
			stderr := captureStderr(t, func() { err = RunRelease(t.TempDir(), tc.args) })
			if err == nil {
				t.Fatal("a retired flag must be a hard error, not silently accepted")
			}
			if !strings.Contains(stderr, "flag provided but not defined") {
				t.Errorf("stderr = %q, want the undefined-flag diagnostic", stderr)
			}
			if len(gh.dispatched) != 0 {
				t.Errorf("must not dispatch on a retired flag, got %v", gh.dispatched)
			}
		})
	}
}

// ── error paths ──────────────────────────────────────────────────────────────

// TestReleaseCIBadFlagError: unknown flag causes fs.Parse to return an error.
func TestReleaseCIBadFlagError(t *testing.T) {
	gh := &fakeCIGH{}
	withCIFakes(t, happyCIGit(), gh)
	err := runReleaseCI(t.TempDir(), []string{"--unknown-flag-xyz"})
	if err == nil {
		t.Fatal("want error for unknown flag, got nil")
	}
	if len(gh.dispatched) != 0 {
		t.Errorf("must not dispatch on flag parse error, got %v", gh.dispatched)
	}
}

// TestReleaseCICurrentBranchError: CurrentBranch failure is surfaced.
func TestReleaseCICurrentBranchError(t *testing.T) {
	git := &fakeCIGit{branchErr: errors.New("not a git repo")}
	gh := &fakeCIGH{}
	withCIFakes(t, git, gh)
	err := runReleaseCI(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "current branch") {
		t.Fatalf("want current-branch error, got %v", err)
	}
	if len(gh.dispatched) != 0 {
		t.Errorf("must not dispatch when CurrentBranch fails")
	}
}

// TestReleaseCIRemoteSHAError: RemoteBranchSHA returning an error (not just empty) is surfaced.
func TestReleaseCIRemoteSHAError(t *testing.T) {
	git := &fakeCIGit{branch: "main", head: ciSHA, remoteErr: errors.New("network error")}
	gh := &fakeCIGH{}
	withCIFakes(t, git, gh)
	err := runReleaseCI(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "resolve origin/main") {
		t.Fatalf("want resolve-origin error, got %v", err)
	}
	if len(gh.dispatched) != 0 {
		t.Errorf("must not dispatch when RemoteBranchSHA errors")
	}
}

// TestReleaseCIHeadSHAError: HeadSHA failure is surfaced.
func TestReleaseCIHeadSHAError(t *testing.T) {
	git := &fakeCIGit{
		branch:  "main",
		headErr: errors.New("rev-parse failed"),
		remote:  map[string]string{"main": ciSHA},
	}
	gh := &fakeCIGH{}
	withCIFakes(t, git, gh)
	err := runReleaseCI(t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "head sha") {
		t.Fatalf("want head-sha error, got %v", err)
	}
	if len(gh.dispatched) != 0 {
		t.Errorf("must not dispatch when HeadSHA errors")
	}
}

// TestReleaseCIRunListErrorAbortsBeforePin: the single run-list query feeds the
// prune, the collision guard and the --watch baseline, so it runs BEFORE the pin
// is created — a failure leaves nothing behind to clean up.
func TestReleaseCIRunListErrorAbortsBeforePin(t *testing.T) {
	git := pinnedCIGit()
	gh := &fakeCIGH{runsBeforeErr: errors.New("baseline api down")}
	withCIFakes(t, git, gh)
	err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA, "--watch"})
	if err == nil || !strings.Contains(err.Error(), "baseline api down") {
		t.Fatalf("want run-list error surfaced, got %v", err)
	}
	if len(git.pushedTags) != 0 || len(git.deletedTags) != 0 {
		t.Errorf("no pin should exist yet: pushed=%v deleted=%v", git.pushedTags, git.deletedTags)
	}
	if len(gh.dispatched) != 0 {
		t.Errorf("must not dispatch when the run list fails")
	}
}

// TestReleaseCICommitDispatchErrorCleansPin: a pin pushed for a dispatch that
// then fails is removed — no run will ever reference it.
func TestReleaseCICommitDispatchErrorCleansPin(t *testing.T) {
	git := pinnedCIGit()
	gh := &fakeCIGH{dispatchErr: errors.New("dispatch failed")}
	withCIFakes(t, git, gh)
	err := runReleaseCI(t.TempDir(), []string{"--commit", pinnedSHA})
	if err == nil || !strings.Contains(err.Error(), "dispatch failed") {
		t.Fatalf("want dispatch error surfaced, got %v", err)
	}
	wantTag := pinTagFor(pinnedSHA)
	if len(git.pushedTags) != 1 || git.pushedTags[0] != wantTag {
		t.Errorf("pushed tags = %v, want [%s]", git.pushedTags, wantTag)
	}
	if len(git.deletedTags) != 1 || git.deletedTags[0] != wantTag {
		t.Errorf("deleted tags = %v, want [%s]; pin tag must be cleaned on dispatch error", git.deletedTags, wantTag)
	}
}

// TestReleaseCIWatchNonTTYPollProgress: non-TTY mode where the first poll has a
// pending platform (triggering the else-if poll%ciNonTTYLogEvery==0 log branch),
// and the second poll resolves it green.
func TestReleaseCIWatchNonTTYPollProgress(t *testing.T) {
	noOpSleep(t)
	withCITTY(t, false)
	var calls int
	gh := &fakeCIGH{
		runsAfter: []ghRun{{DatabaseID: 1, HeadSHA: ciSHA}},
		runJobsFn: func(int64) ([]ghJob, error) {
			calls++
			if calls == 1 {
				// poll==0: 0 % 3 == 0, so the non-TTY log branch executes
				return []ghJob{{Name: "linux-amd64"}}, nil // no conclusion → absent
			}
			return []ghJob{{Name: "linux-amd64", Conclusion: "success"}}, nil
		},
	}
	withCIFakes(t, happyCIGit(), gh)
	if err := runReleaseCI(t.TempDir(), []string{"--watch"}); err != nil {
		t.Fatalf("non-TTY poll-progress watch should succeed: %v", err)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 RunJobs calls (one pending, one green), got %d", calls)
	}
}

// TestReleaseCIWatchAlreadyDecidedPlatformSkipped: when two runs at the same SHA
// both provide jobs for the same platform, the second run's result is ignored once
// the platform is already decided (exercises the cur != ciAbsent guard in
// ciStatusFromNewRuns).
func TestReleaseCIWatchAlreadyDecidedPlatformSkipped(t *testing.T) {
	noOpSleep(t)
	// Run 2 (higher ID) is listed first so it would win if the guard were absent;
	// run 1 also covers linux-amd64. The guard must keep whichever was applied first.
	run1 := ghRun{DatabaseID: 1, HeadSHA: ciSHA}
	run2 := ghRun{DatabaseID: 2, HeadSHA: ciSHA}
	gh := &fakeCIGH{
		runsAfter: []ghRun{run1, run2},
		jobs: map[int64][]ghJob{
			1: {{Name: "linux-amd64", Conclusion: "success"}},
			// run2 has the same platform — second encounter must be skipped
			2: {{Name: "linux-amd64", Conclusion: "failure"}},
		},
	}
	withCIFakes(t, happyCIGit(), gh)
	// The watch should succeed: run1's success is applied and run2's failure for
	// the same platform is ignored.
	if err := runReleaseCI(t.TempDir(), []string{"--watch"}); err != nil {
		t.Fatalf("want green (first run wins), got: %v", err)
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
