package common

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// release_cut_test.go is the hermetic test suite for the gated `bin/release cut`
// orchestrator (T0943). It swaps the git/gh/uploader seams for in-memory fakes
// so no `git`/`gh` process is ever spawned. The CI watch loop's sleep is no-op'd
// via sleepFn.

// ── fakes ───────────────────────────────────────────────────────────────────

type createdTag struct {
	tag, message, sha string
	force             bool
}

// fakeCutGit is an in-memory cutGit. It records every mutating call so tests can
// assert the tag → push → bump → commit → push ordering and contents.
type fakeCutGit struct {
	head        string
	branch      string
	branchErr   error
	clean       bool
	ancestorOK  bool
	fetchErr    error
	tags        map[string]string // tag → sha (existence + TagSHA)
	epochTags   []string
	resolved    map[string]string // --commit commit-ish → full sha (ResolveSHA)
	logSubjects []string          // commit subjects LogSubjects returns

	remotePinTags []string        // ci-pin-* tags ListRemotePinTags reports
	pushedPinTags []string        // tags pushed via PushTagAt
	deletedTags   []string        // tags deleted via DeletePinTag
	notOnOrigin   map[string]bool // shas ReachableFromOrigin reports as absent
	pushPinErr    error
	deletePinErr  error
	listPinErr    error
	reachableErr  error

	// Injected tool-failure errors, so tests can cover the "underlying git call
	// errored → gate hard-fails (non-overridable)" contract.
	cleanErr     error
	ancestorErr  error
	headErr      error
	listEpochErr error
	createTagErr error
	resolveErr   error
	logErr       error

	createdTags    []createdTag
	pushedTags     []string
	committed      []string // commit messages
	addedFiles     []string
	pushedBranches []string
}

func newFakeCutGit() *fakeCutGit {
	return &fakeCutGit{
		head: "abcdef0123456789abcdef0123456789abcdef01", branch: "main",
		clean: true, ancestorOK: true,
		tags: map[string]string{}, resolved: map[string]string{},
	}
}

func (g *fakeCutGit) HeadSHA() (string, error)       { return g.head, g.headErr }
func (g *fakeCutGit) CurrentBranch() (string, error) { return g.branch, g.branchErr }
func (g *fakeCutGit) CleanTree() (bool, error)       { return g.clean, g.cleanErr }
func (g *fakeCutGit) IsAncestor(sha, ref string) (bool, error) {
	return g.ancestorOK, g.ancestorErr
}
func (g *fakeCutGit) Fetch() error { return g.fetchErr }
func (g *fakeCutGit) TagExists(tag string) (bool, error) {
	_, ok := g.tags[tag]
	return ok, nil
}
func (g *fakeCutGit) TagSHA(tag string) (string, error) {
	sha, ok := g.tags[tag]
	if !ok {
		return "", os.ErrNotExist
	}
	return sha, nil
}
func (g *fakeCutGit) ListEpochTags() ([]string, error) { return g.epochTags, g.listEpochErr }
func (g *fakeCutGit) CreateTag(tag, message, sha string, force bool) error {
	if g.createTagErr != nil {
		return g.createTagErr
	}
	g.createdTags = append(g.createdTags, createdTag{tag, message, sha, force})
	g.tags[tag] = sha
	return nil
}
func (g *fakeCutGit) PushTag(tag string, force bool) error {
	g.pushedTags = append(g.pushedTags, tag)
	return nil
}
func (g *fakeCutGit) CommitFile(path, message string) error {
	g.addedFiles = append(g.addedFiles, path)
	g.committed = append(g.committed, message)
	return nil
}
func (g *fakeCutGit) PushBranch(branch string) error {
	g.pushedBranches = append(g.pushedBranches, branch)
	return nil
}
func (g *fakeCutGit) ResolveSHA(ref string) (string, error) {
	if g.resolveErr != nil {
		return "", g.resolveErr
	}
	if sha, ok := g.resolved[ref]; ok {
		return sha, nil
	}
	return ref, nil // unmapped refs resolve to themselves (e.g. a literal sha)
}
func (g *fakeCutGit) LogSubjects(fromRef, toSHA string) ([]string, error) {
	return g.logSubjects, g.logErr
}
func (g *fakeCutGit) PushTagAt(tag, _ string) error {
	if g.pushPinErr != nil {
		return g.pushPinErr
	}
	g.pushedPinTags = append(g.pushedPinTags, tag)
	return nil
}
func (g *fakeCutGit) DeletePinTag(tag string) error {
	if g.deletePinErr != nil {
		return g.deletePinErr
	}
	g.deletedTags = append(g.deletedTags, tag)
	return nil
}
func (g *fakeCutGit) ListRemotePinTags() ([]string, error) {
	return g.remotePinTags, g.listPinErr
}

// ReachableFromOrigin defaults to "yes" — the normal state for a cut target, so
// only the tests that exercise the unpushed-commit refusal opt out.
func (g *fakeCutGit) ReachableFromOrigin(sha string) (bool, error) {
	return !g.notOnOrigin[sha], g.reachableErr
}

// fakeCutGH is an in-memory cutGH. ci.yml runs flip from ciRunsBefore to
// ciRunsAfter once a dispatch happens, so the dispatch-and-watch path can be
// exercised without a real workflow.
type fakeCutGH struct {
	mu             sync.Mutex
	ciRunsBefore   []ghRun
	ciRunsAfter    []ghRun
	releaseRuns    []ghRun
	jobs           map[int64][]ghJob
	jobsFn         func(int64) ([]ghJob, error) // overrides the jobs map when set
	dispatched     []map[string]string
	dispatchedRefs []string // the ref each dispatch targeted, index-aligned with dispatched
	dispatchedFlag bool
	dispatchErr    error
	runsErr        error // injected `gh run list` failure
	runsErrAfter   error // injected `gh run list` failure, but only once a dispatch has happened
	runsCalls      int   // WorkflowRuns call count, so a test can fail a specific one
	runsErrFrom    int   // 1-based call number from which WorkflowRuns fails (0 = never)
	jobsErr        error // injected `gh run view` failure
	cancelled      []int64
	cancelErr      error
}

func (f *fakeCutGH) CancelRun(runID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancelled = append(f.cancelled, runID)
	return nil
}

func (f *fakeCutGH) WorkflowRuns(workflow string, limit int) ([]ghRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runsCalls++
	if f.runsErr != nil {
		return nil, f.runsErr
	}
	if f.runsErrFrom != 0 && f.runsCalls >= f.runsErrFrom {
		return nil, fmt.Errorf("run list failed on call %d", f.runsCalls)
	}
	if f.runsErrAfter != nil && f.dispatchedFlag {
		return nil, f.runsErrAfter
	}
	if workflow == "release.yml" {
		return f.releaseRuns, nil
	}
	if f.dispatchedFlag && f.ciRunsAfter != nil {
		return f.ciRunsAfter, nil
	}
	return f.ciRunsBefore, nil
}
func (f *fakeCutGH) RunJobs(runID int64) ([]ghJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jobsErr != nil {
		return nil, f.jobsErr
	}
	if f.jobsFn != nil {
		return f.jobsFn(runID)
	}
	return f.jobs[runID], nil
}
func (f *fakeCutGH) DispatchWorkflow(workflow, ref string, inputs map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dispatchErr != nil {
		return f.dispatchErr
	}
	f.dispatched = append(f.dispatched, inputs)
	f.dispatchedRefs = append(f.dispatchedRefs, ref)
	f.dispatchedFlag = true
	return nil
}

// lastPlatform is the platform these tests single out when they need exactly
// one member of the matrix to be missing or failing. Derived from the end of
// requiredPlatforms rather than a literal index so that adding a platform (e.g.
// linux-arm64) cannot silently change which case is under test — or, worse,
// leave a test asserting "one absent" while two are.
func lastPlatform() string { return requiredPlatforms[len(requiredPlatforms)-1] }

// allButLastGreen returns a successful job for every required platform except
// the last, i.e. a matrix with exactly one platform absent.
func allButLastGreen() []ghJob {
	jobs := make([]ghJob, 0, len(requiredPlatforms)-1)
	for _, p := range requiredPlatforms[:len(requiredPlatforms)-1] {
		jobs = append(jobs, ghJob{Name: p, Status: "completed", Conclusion: "success"})
	}
	return jobs
}

// olderRunReportingLinuxFailed is the second, older run used by the
// newest-run-wins test: it re-reports requiredPlatforms[0] as a failure (which
// must be ignored, the newer run already decided it green) and supplies a green
// job for every other platform.
func olderRunReportingLinuxFailed() []ghJob {
	jobs := []ghJob{{Name: requiredPlatforms[0], Conclusion: "failure"}}
	for _, p := range requiredPlatforms[1:] {
		jobs = append(jobs, ghJob{Name: p, Conclusion: "success"})
	}
	return jobs
}

// greenCI builds a single ci.yml run at sha whose jobs all succeed.
func greenCI(sha string) *fakeCutGH {
	jobs := make([]ghJob, len(requiredPlatforms))
	for i, p := range requiredPlatforms {
		jobs[i] = ghJob{Name: p, Status: "completed", Conclusion: "success"}
	}
	return &fakeCutGH{
		ciRunsBefore: []ghRun{{DatabaseID: 1, HeadSHA: sha, HeadBranch: "main", Status: "completed", Conclusion: "success"}},
		jobs:         map[int64][]ghJob{1: jobs},
	}
}

// noOpSleep replaces the CI watch sleep so dispatch tests don't actually wait.
func noOpSleep(t *testing.T) {
	t.Helper()
	prev := sleepFn
	sleepFn = func(time.Duration) {}
	t.Cleanup(func() { sleepFn = prev })
}

// ── epoch derivation ─────────────────────────────────────────────────────────

func TestDeriveStableTarget(t *testing.T) {
	cases := []struct {
		name       string
		last       epoch
		haveLast   bool
		year       int
		want       epoch
		wantConfir bool
		wantErr    error // nil | errMultiYearGap | errClockBehind
	}{
		{"first release", epoch{}, false, 2026, epoch{2026, 0}, false, nil},
		{"same-year increment", epoch{2026, 0}, true, 2026, epoch{2026, 1}, false, nil},
		{"same-year increment N>0", epoch{2026, 3}, true, 2026, epoch{2026, 4}, false, nil},
		{"year rollover", epoch{2026, 5}, true, 2027, epoch{2027, 0}, true, nil},
		{"multi-year gap", epoch{2024, 2}, true, 2026, epoch{2026, 0}, true, errMultiYearGap},
		{"clock behind", epoch{2027, 1}, true, 2026, epoch{}, false, errClockBehind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, confirm, err := deriveStableTarget(tc.last, tc.haveLast, tc.year)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error %v, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr.Error()) {
					t.Fatalf("error = %v, want wrapping %v", err, tc.wantErr)
				}
			}
			if got != tc.want {
				t.Errorf("target = %v, want %v", got, tc.want)
			}
			if confirm != tc.wantConfir {
				t.Errorf("needYearConfirm = %v, want %v", confirm, tc.wantConfir)
			}
		})
	}
}

func TestHighestReleasedEpoch(t *testing.T) {
	g := newFakeCutGit()
	g.epochTags = []string{"epoch-2026.0", "epoch-next", "epoch-2026.2", "epoch-2025.7", "epoch-garbage"}
	last, ok, err := highestReleasedEpoch(g)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected a highest epoch")
	}
	if last != (epoch{2026, 2}) {
		t.Fatalf("highest = %v, want 2026.2 (epoch-next + non-numeric ignored)", last)
	}

	// No epoch tags → ok=false.
	g.epochTags = []string{"epoch-next"}
	if _, ok, _ := highestReleasedEpoch(g); ok {
		t.Fatal("expected ok=false when only epoch-next exists")
	}
}

// ── ciStatusAtSHA ─────────────────────────────────────────────────────────────

func TestCIStatusAtSHA(t *testing.T) {
	sha := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	t.Run("all green", func(t *testing.T) {
		status, err := ciStatusAtSHA(greenCI(sha), sha)
		if err != nil {
			t.Fatal(err)
		}
		failed, absent := splitCIStatus(status)
		if len(failed) != 0 || len(absent) != 0 {
			t.Fatalf("failed=%v absent=%v, want all green", failed, absent)
		}
	})

	t.Run("one failed", func(t *testing.T) {
		gh := greenCI(sha)
		gh.jobs[1][len(requiredPlatforms)-1] = ghJob{Name: lastPlatform(), Conclusion: "failure"}
		status, _ := ciStatusAtSHA(gh, sha)
		failed, _ := splitCIStatus(status)
		if len(failed) != 1 || failed[0] != lastPlatform() {
			t.Fatalf("failed = %v, want [%s]", failed, lastPlatform())
		}
	})

	t.Run("one absent", func(t *testing.T) {
		gh := greenCI(sha)
		gh.jobs[1] = allButLastGreen() // drop the last platform's job
		status, _ := ciStatusAtSHA(gh, sha)
		_, absent := splitCIStatus(status)
		if len(absent) != 1 || absent[0] != lastPlatform() {
			t.Fatalf("absent = %v, want [%s]", absent, lastPlatform())
		}
	})

	t.Run("multiple runs aggregated", func(t *testing.T) {
		// One per-platform run each — must aggregate to all-green.
		runs := make([]ghRun, 0, len(requiredPlatforms))
		jobs := make(map[int64][]ghJob, len(requiredPlatforms))
		for i, p := range requiredPlatforms {
			id := int64(i + 1)
			runs = append(runs, ghRun{DatabaseID: id, HeadSHA: sha})
			jobs[id] = []ghJob{{Name: p, Conclusion: "success"}}
		}
		gh := &fakeCutGH{ciRunsBefore: runs, jobs: jobs}
		status, _ := ciStatusAtSHA(gh, sha)
		failed, absent := splitCIStatus(status)
		if len(failed) != 0 || len(absent) != 0 {
			t.Fatalf("failed=%v absent=%v, want all green via aggregation", failed, absent)
		}
	})

	t.Run("different SHA ignored", func(t *testing.T) {
		gh := greenCI("othersha")
		// Add a run at the target SHA covering only linux.
		gh.ciRunsBefore = append(gh.ciRunsBefore, ghRun{DatabaseID: 9, HeadSHA: sha})
		gh.jobs[9] = []ghJob{{Name: requiredPlatforms[0], Conclusion: "success"}}
		status, _ := ciStatusAtSHA(gh, sha)
		_, absent := splitCIStatus(status)
		// Every platform but linux has no run at sha → absent.
		if len(absent) != len(requiredPlatforms)-1 {
			t.Fatalf("absent = %v, want every platform but %s (other-SHA run ignored)",
				absent, requiredPlatforms[0])
		}
	})
}

// ── gate 7 state machine ──────────────────────────────────────────────────────

func baseCutCtx(gh cutGH, git cutGit) *cutContext {
	return &cutContext{
		channel:     "stable",
		targetSHA:   "abcdef0123456789abcdef0123456789abcdef01",
		targetEpoch: epoch{2026, 1},
		gh:          gh, git: git,
		stdout: &strings.Builder{},
	}
}

func TestGateCIGreen(t *testing.T) {
	ctx := baseCutCtx(greenCI("abcdef0123456789abcdef0123456789abcdef01"), newFakeCutGit())
	res := gateCI(ctx)
	if !res.passed {
		t.Fatalf("green CI must pass: %s", res.detail)
	}
}

func TestGateCIFailedRefusesOverridable(t *testing.T) {
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	gh := greenCI(sha)
	gh.jobs[1][0] = ghJob{Name: requiredPlatforms[0], Conclusion: "failure"}
	res := gateCI(baseCutCtx(gh, newFakeCutGit()))
	if res.passed || !res.overridable {
		t.Fatalf("failed CI must refuse (overridable): %+v", res)
	}
	if !strings.Contains(res.detail, "CI failed") {
		t.Fatalf("detail = %q", res.detail)
	}
}

func TestGateCIAbsentRunCIDispatchesAndWatches(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	gh := &fakeCutGH{
		ciRunsBefore: nil, // absent for everything
		ciRunsAfter:  greenCI(sha).ciRunsBefore,
		jobs:         greenCI(sha).jobs,
	}
	ctx := baseCutCtx(gh, newFakeCutGit())
	ctx.runCI = true
	res := gateCI(ctx)
	if !res.passed {
		t.Fatalf("absent + --run-ci must dispatch, watch, and pass: %s", res.detail)
	}
	if len(gh.dispatched) == 0 {
		t.Fatal("expected a ci.yml dispatch")
	}
	// Whole matrix absent → single platform=all dispatch.
	if gh.dispatched[0]["platform"] != "all" {
		t.Fatalf("dispatch platform = %q, want all", gh.dispatched[0]["platform"])
	}
}

func TestGateCIDryRunNeverDispatches(t *testing.T) {
	// --dry-run must change nothing — even with --run-ci and an absent run it must
	// NOT dispatch a real ci.yml workflow.
	gh := &fakeCutGH{ciRunsBefore: nil} // absent for everything
	ctx := baseCutCtx(gh, newFakeCutGit())
	ctx.runCI = true
	ctx.dryRun = true
	res := gateCI(ctx)
	if res.passed || !res.overridable {
		t.Fatalf("dry-run absent CI must report overridably without dispatching: %+v", res)
	}
	if len(gh.dispatched) != 0 {
		t.Fatalf("--dry-run must not dispatch ci.yml, got %v", gh.dispatched)
	}
	if !strings.Contains(res.detail, "dry-run") {
		t.Fatalf("detail = %q, want a dry-run note", res.detail)
	}
}

func TestGateCIAbsentNoCIWaitStops(t *testing.T) {
	gh := &fakeCutGH{ciRunsBefore: nil}
	ctx := baseCutCtx(gh, newFakeCutGit())
	ctx.runCI = true
	ctx.noCIWait = true
	res := gateCI(ctx)
	if res.passed {
		t.Fatal("--no-ci-wait must not pass the gate")
	}
	if res.overridable {
		t.Fatal("--no-ci-wait is a hard stop, not overridable")
	}
	if len(gh.dispatched) == 0 {
		t.Fatal("expected a dispatch before stopping")
	}
	if !strings.Contains(res.detail, "re-run") {
		t.Fatalf("detail = %q, want a 're-run cut once green' message", res.detail)
	}
}

func TestGateCIAbsentNonInteractiveNamesPlatforms(t *testing.T) {
	gh := &fakeCutGH{ciRunsBefore: nil}
	ctx := baseCutCtx(gh, newFakeCutGit())
	// no --run-ci, not interactive → must name platforms, not bare-refuse.
	res := gateCI(ctx)
	if res.passed || !res.overridable {
		t.Fatalf("absent non-interactive must fail overridably: %+v", res)
	}
	for _, p := range requiredPlatforms {
		if !strings.Contains(res.detail, p) {
			t.Fatalf("detail %q must name platform %s", res.detail, p)
		}
	}
	if len(gh.dispatched) != 0 {
		t.Fatal("must not dispatch without --run-ci / confirmation")
	}
}

func TestGateCIAbsentInteractiveYesDispatches(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	// Before dispatch: run 1 covers every platform but the last. After dispatch:
	// a second run adds the missing job, so the matrix is complete.
	gh := &fakeCutGH{
		ciRunsBefore: []ghRun{{DatabaseID: 1, HeadSHA: sha}},
		ciRunsAfter:  []ghRun{{DatabaseID: 1, HeadSHA: sha}, {DatabaseID: 2, HeadSHA: sha}},
		jobs: map[int64][]ghJob{
			1: allButLastGreen(),
			2: {{Name: lastPlatform(), Conclusion: "success"}},
		},
	}
	ctx := baseCutCtx(gh, newFakeCutGit())
	ctx.interactive = true
	ctx.stdin = strings.NewReader("y\n")
	res := gateCI(ctx)
	if !res.passed {
		t.Fatalf("interactive 'y' must dispatch + pass: %s", res.detail)
	}
	if len(gh.dispatched) != 1 || gh.dispatched[0]["platform"] != lastPlatform() {
		t.Fatalf("expected a single %s dispatch, got %v", lastPlatform(), gh.dispatched)
	}
}

func TestGateCIAbsentInteractiveNoDeclines(t *testing.T) {
	gh := greenCI("abcdef0123456789abcdef0123456789abcdef01")
	gh.jobs[1] = allButLastGreen() // last platform absent
	ctx := baseCutCtx(gh, newFakeCutGit())
	ctx.interactive = true
	ctx.stdin = strings.NewReader("n\n")
	res := gateCI(ctx)
	if res.passed || !res.overridable {
		t.Fatalf("interactive 'n' must decline (overridable): %+v", res)
	}
	if len(gh.dispatched) != 0 {
		t.Fatal("declining must not dispatch")
	}
}

// ── individual gates ──────────────────────────────────────────────────────────

func TestGateCleanTree(t *testing.T) {
	g := newFakeCutGit()
	if res := gateCleanTree(&cutContext{git: g}); !res.passed {
		t.Fatalf("clean tree must pass: %s", res.detail)
	}
	g.clean = false
	if res := gateCleanTree(&cutContext{git: g}); res.passed || !res.overridable {
		t.Fatalf("dirty tree must fail overridably: %+v", res)
	}
}

func TestGateReachable(t *testing.T) {
	g := newFakeCutGit()
	ctx := &cutContext{git: g, targetSHA: g.head}
	if res := gateReachable(ctx); !res.passed {
		t.Fatalf("ancestor must pass: %s", res.detail)
	}
	g.ancestorOK = false
	if res := gateReachable(ctx); res.passed || !res.overridable {
		t.Fatalf("non-ancestor must fail overridably: %+v", res)
	}
}

func TestGateCatalogEpoch(t *testing.T) {
	root, _ := fakeReleaseRoot(t, map[string]string{})
	writeCatalogFile(t, root, "2026.1")
	if res := gateCatalogEpoch(&cutContext{root: root, targetEpoch: epoch{2026, 1}}); !res.passed {
		t.Fatalf("matching epoch must pass: %s", res.detail)
	}
	res := gateCatalogEpoch(&cutContext{root: root, targetEpoch: epoch{2026, 2}})
	if res.passed || !res.overridable {
		t.Fatalf("mismatched epoch must fail overridably: %+v", res)
	}
	if !strings.Contains(res.detail, "catalog epoch is 2026.1, expected target 2026.2") {
		t.Fatalf("detail = %q", res.detail)
	}
}

func TestGateDepsHosted(t *testing.T) {
	root, shas, uploader := depsHostedFixture(t, true)
	ctx := &cutContext{root: root, uploader: uploader}
	if res := gateDepsHosted(ctx); !res.passed {
		t.Fatalf("all blobs hosted must pass: %s", res.detail)
	}
	_ = shas

	// Drop one hosted asset → fail overridably, naming the gap.
	root2, _, uploader2 := depsHostedFixture(t, false)
	res := gateDepsHosted(&cutContext{root: root2, uploader: uploader2})
	if res.passed || !res.overridable {
		t.Fatalf("missing hosted blob must fail overridably: %+v", res)
	}
	if !strings.Contains(res.detail, "not hosted") {
		t.Fatalf("detail = %q", res.detail)
	}
}

func TestGateStableTagAbsent(t *testing.T) {
	g := newFakeCutGit()
	ctx := &cutContext{git: g, targetEpoch: epoch{2026, 1}}
	if res := gateStableTagAbsent(ctx); !res.passed {
		t.Fatalf("absent tag must pass: %s", res.detail)
	}
	g.tags["epoch-2026.1"] = "somesha"
	res := gateStableTagAbsent(ctx)
	if res.passed {
		t.Fatal("present stable tag must fail")
	}
	if res.overridable {
		t.Fatal("stable-tag immutability must NOT be overridable")
	}
}

func TestGateEpochNextValidated(t *testing.T) {
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	g := newFakeCutGit()
	g.head = sha
	g.tags["epoch-next"] = sha
	gh := &fakeCutGH{releaseRuns: []ghRun{
		{DatabaseID: 7, HeadSHA: sha, HeadBranch: "epoch-next", Conclusion: "success"},
	}}
	ctx := &cutContext{git: g, gh: gh, targetSHA: sha}
	if res := gateEpochNextValidated(ctx); !res.passed {
		t.Fatalf("validated epoch-next must pass: %s", res.detail)
	}

	// epoch-next at a different commit → fail overridably.
	g.tags["epoch-next"] = "othersha"
	if res := gateEpochNextValidated(ctx); res.passed || !res.overridable {
		t.Fatalf("epoch-next at wrong SHA must fail overridably: %+v", res)
	}

	// Right SHA but no successful release run → fail overridably.
	g.tags["epoch-next"] = sha
	gh.releaseRuns = []ghRun{{DatabaseID: 7, HeadSHA: sha, HeadBranch: "epoch-next", Conclusion: "failure"}}
	if res := gateEpochNextValidated(ctx); res.passed || !res.overridable {
		t.Fatalf("failed epoch-next release must fail overridably: %+v", res)
	}
}

func TestGenerateReleaseNotes(t *testing.T) {
	// With a previous epoch: range-scoped header + a bullet per commit subject.
	g := newFakeCutGit()
	g.logSubjects = []string{"T0975: fix double-free", "B0120: park_m wakeup"}
	ctx := &cutContext{git: g, targetSHA: "deadbeef", lastEpoch: epoch{2026, 0}, haveLast: true}
	notes, err := generateReleaseNotes(ctx)
	if err != nil {
		t.Fatalf("generateReleaseNotes: %v", err)
	}
	for _, want := range []string{"**Install:**", "docs/installing.md", "since epoch-2026.0", "(2 commits)", "- T0975: fix double-free", "- B0120: park_m wakeup"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("notes missing %q:\n%s", want, notes)
		}
	}

	// First release (no previous epoch): un-scoped header.
	g.logSubjects = []string{"initial commit"}
	ctx.haveLast = false
	notes, err = generateReleaseNotes(ctx)
	if err != nil {
		t.Fatalf("generateReleaseNotes (first): %v", err)
	}
	if strings.Contains(notes, "since") || !strings.Contains(notes, "(1 commit)") {
		t.Fatalf("first-release notes = %q", notes)
	}

	// Empty range still produces a body (no crash, explicit note).
	g.logSubjects = nil
	if notes, err = generateReleaseNotes(ctx); err != nil || !strings.Contains(notes, "no commits") {
		t.Fatalf("empty-range notes = %q (%v)", notes, err)
	}

	// A LogSubjects tool error propagates.
	g.logErr = errors.New("git log exploded")
	if _, err := generateReleaseNotes(ctx); err == nil {
		t.Fatal("a LogSubjects error must propagate")
	}
}

// ── cut stable: happy path + dry-run + override audit ─────────────────────────

func TestCutStableHappyPath(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")

	g := newFakeCutGit()
	g.head = sha
	g.epochTags = []string{"epoch-2026.0"}
	g.tags["epoch-2026.0"] = "oldsha"
	g.tags["epoch-next"] = sha

	gh := greenCI(sha)
	gh.releaseRuns = []ghRun{{DatabaseID: 7, HeadSHA: sha, HeadBranch: "epoch-next", Conclusion: "success"}}

	ctx := stableCtx(root, g, gh, uploader)
	withYear(t, 2026)
	if err := cutStable(ctx); err != nil {
		t.Fatalf("happy-path cut stable failed: %v", err)
	}

	// Tag created (not forced) at the release SHA, then pushed.
	if len(g.createdTags) != 1 || g.createdTags[0].tag != "epoch-2026.1" {
		t.Fatalf("createdTags = %+v, want one epoch-2026.1", g.createdTags)
	}
	if g.createdTags[0].force {
		t.Fatal("stable tag must NOT be force-created")
	}
	if g.createdTags[0].sha != sha {
		t.Fatalf("tagged sha = %s, want release SHA %s", g.createdTags[0].sha, sha)
	}
	if len(g.pushedTags) != 1 || g.pushedTags[0] != "epoch-2026.1" {
		t.Fatalf("pushedTags = %v", g.pushedTags)
	}
	// Catalog bumped to next same-year epoch + committed + branch pushed.
	if got, _ := ParseEpoch(root); got != "2026.2" {
		t.Fatalf("catalog epoch after bump = %s, want 2026.2", got)
	}
	if len(g.committed) != 1 || !strings.Contains(g.committed[0], "2026.2") {
		t.Fatalf("commit = %v, want a 2026.2 bump", g.committed)
	}
	if len(g.addedFiles) != 1 || g.addedFiles[0] != "catalog.toml" {
		t.Fatalf("addedFiles = %v, want [catalog.toml]", g.addedFiles)
	}
	if len(g.pushedBranches) != 1 || g.pushedBranches[0] != "main" {
		t.Fatalf("pushedBranches = %v, want [main]", g.pushedBranches)
	}
}

func TestCutStableDryRunNoMutations(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")

	g := newFakeCutGit()
	g.head = sha
	g.epochTags = []string{"epoch-2026.0"}
	g.tags["epoch-next"] = sha
	gh := greenCI(sha)
	gh.releaseRuns = []ghRun{{DatabaseID: 7, HeadSHA: sha, HeadBranch: "epoch-next", Conclusion: "success"}}

	ctx := stableCtx(root, g, gh, uploader)
	ctx.dryRun = true
	withYear(t, 2026)
	if err := cutStable(ctx); err != nil {
		t.Fatalf("dry-run cut stable should pass gates and report: %v", err)
	}
	if len(g.createdTags) != 0 || len(g.pushedTags) != 0 || len(g.committed) != 0 || len(g.pushedBranches) != 0 {
		t.Fatalf("--dry-run made mutations: tags=%v pushed=%v commits=%v branches=%v",
			g.createdTags, g.pushedTags, g.committed, g.pushedBranches)
	}
	if got, _ := ParseEpoch(root); got != "2026.1" {
		t.Fatalf("--dry-run rewrote catalog (epoch=%s, want 2026.1)", got)
	}
}

func TestCutStableReasonOverrideAudit(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")

	// ancestorOK=false → gateReachable fails (overridable).
	g := newFakeCutGit()
	g.head = sha
	g.ancestorOK = false
	g.epochTags = []string{"epoch-2026.0"}
	g.tags["epoch-next"] = sha
	gh := greenCI(sha)
	gh.releaseRuns = []ghRun{{DatabaseID: 7, HeadSHA: sha, HeadBranch: "epoch-next", Conclusion: "success"}}

	// Without --reason: aborts.
	ctx := stableCtx(root, g, gh, uploader)
	withYear(t, 2026)
	if err := cutStable(ctx); err == nil {
		t.Fatal("an unreachable tag commit must abort without --reason")
	}
	if len(g.createdTags) != 0 {
		t.Fatal("no tag must be created on a failed gate")
	}

	// With --reason: proceeds, and the reason lands in the tag message.
	g2 := newFakeCutGit()
	g2.head = sha
	g2.ancestorOK = false
	g2.epochTags = []string{"epoch-2026.0"}
	g2.tags["epoch-next"] = sha
	ctx2 := stableCtx(root, g2, gh, uploader)
	ctx2.reason = "cutting from a vendored snapshot for this release"
	if err := cutStable(ctx2); err != nil {
		t.Fatalf("--reason should bypass the overridable gate: %v", err)
	}
	if len(g2.createdTags) != 1 {
		t.Fatalf("expected the cut to proceed, createdTags=%v", g2.createdTags)
	}
	if !strings.Contains(g2.createdTags[0].message, "cutting from a vendored snapshot") {
		t.Fatalf("tag message must record the override reason, got: %q", g2.createdTags[0].message)
	}
}

func TestCutStableClockBehindRefuses(t *testing.T) {
	root, _, uploader := depsHostedFixture(t, true)
	g := newFakeCutGit()
	g.epochTags = []string{"epoch-2027.0"} // last is ahead of the device clock
	ctx := stableCtx(root, g, greenCI(g.head), uploader)
	ctx.reason = "I really want to" // must NOT help
	withYear(t, 2026)
	err := cutStable(ctx)
	if err == nil {
		t.Fatal("clock-behind must refuse")
	}
	if !strings.Contains(err.Error(), "never cut backward") {
		t.Fatalf("error = %v, want clock-behind refusal", err)
	}
	if len(g.createdTags) != 0 {
		t.Fatal("clock-behind must not tag")
	}
}

func TestCutStableMultiYearGapNeedsReason(t *testing.T) {
	root, _, uploader := depsHostedFixture(t, true)
	g := newFakeCutGit()
	g.epochTags = []string{"epoch-2024.0"}
	withYear(t, 2026)

	ctx := stableCtx(root, g, greenCI(g.head), uploader)
	if err := cutStable(ctx); err == nil {
		t.Fatal("multi-year gap must refuse without --reason")
	} else if !strings.Contains(err.Error(), "multi-year gap") {
		t.Fatalf("error = %v, want multi-year-gap refusal", err)
	}
}

// ── cut next ──────────────────────────────────────────────────────────────────

func TestCutNextHappyPath(t *testing.T) {
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")
	g := newFakeCutGit()
	g.head = sha
	gh := greenCI(sha)

	ctx := &cutContext{
		root: root, channel: "next",
		git: g, gh: gh, uploader: uploader,
		stdout: &strings.Builder{},
	}
	if err := cutNext(ctx); err != nil {
		t.Fatalf("cut next happy path failed: %v", err)
	}
	if len(g.createdTags) != 1 || g.createdTags[0].tag != "epoch-next" {
		t.Fatalf("createdTags = %+v, want one epoch-next", g.createdTags)
	}
	if !g.createdTags[0].force {
		t.Fatal("epoch-next must be force-created (moving tag)")
	}
	if len(g.pushedTags) != 1 || g.pushedTags[0] != "epoch-next" {
		t.Fatalf("pushedTags = %v, want [epoch-next]", g.pushedTags)
	}
}

func TestCutNextDryRunNoMutations(t *testing.T) {
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")
	g := newFakeCutGit()
	g.head = sha
	ctx := &cutContext{
		root: root, channel: "next", dryRun: true,
		git: g, gh: greenCI(sha), uploader: uploader,
		stdout: &strings.Builder{},
	}
	if err := cutNext(ctx); err != nil {
		t.Fatalf("cut next --dry-run failed: %v", err)
	}
	if len(g.createdTags) != 0 || len(g.pushedTags) != 0 {
		t.Fatalf("--dry-run mutated: tags=%v pushed=%v", g.createdTags, g.pushedTags)
	}
}

// TestCutNextDryRunRunCINoDispatch guards the preflight ordering: gateCI runs
// inside preflight (before cutNext's dry-run plan print), so a --dry-run --run-ci
// on an uncovered SHA must still not dispatch a real ci.yml run.
func TestCutNextDryRunRunCINoDispatch(t *testing.T) {
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")
	g := newFakeCutGit()
	gh := &fakeCutGH{ciRunsBefore: nil} // absent for everything
	ctx := &cutContext{
		root: root, channel: "next", dryRun: true, runCI: true,
		git: g, gh: gh, uploader: uploader,
		stdout: &strings.Builder{},
	}
	// Absent CI without --reason makes preflight abort; that is fine — the point
	// is that the abort happens WITHOUT a dispatch or any tag mutation.
	_ = cutNext(ctx)
	if len(gh.dispatched) != 0 {
		t.Fatalf("--dry-run must not dispatch ci.yml, got %v", gh.dispatched)
	}
	if len(g.createdTags) != 0 || len(g.pushedTags) != 0 {
		t.Fatalf("--dry-run mutated: tags=%v pushed=%v", g.createdTags, g.pushedTags)
	}
}

// ── runReleaseCut dispatch + writeCatalogEpoch ────────────────────────────────

func TestRunReleaseCutUnknownChannel(t *testing.T) {
	if err := runReleaseCut(t.TempDir(), []string{"sideways"}); err == nil {
		t.Fatal("unknown channel must error")
	}
	if err := runReleaseCut(t.TempDir(), nil); err == nil {
		t.Fatal("no channel must error")
	}
}

func TestWriteCatalogEpoch(t *testing.T) {
	root := t.TempDir()
	original := "[catalog]\nepoch = \"2026.1\"\n\n[modules.std]\ndescription = \"x\"\n"
	if err := os.WriteFile(filepath.Join(root, "catalog.toml"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeCatalogEpoch(root, epoch{2026, 2}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "catalog.toml"))
	if err != nil {
		t.Fatal(err)
	}
	want := "[catalog]\nepoch = \"2026.2\"\n\n[modules.std]\ndescription = \"x\"\n"
	if string(got) != want {
		t.Fatalf("rewrite changed unrelated lines:\n got: %q\nwant: %q", got, want)
	}
	if e, _ := ParseEpoch(root); e != "2026.2" {
		t.Fatalf("ParseEpoch after rewrite = %s, want 2026.2", e)
	}

	// No epoch line → error.
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "catalog.toml"), []byte("[catalog]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeCatalogEpoch(bad, epoch{2026, 2}); err == nil {
		t.Fatal("expected error when there is no epoch line to rewrite")
	}

	// Missing catalog.toml entirely → the ReadFile error surfaces.
	if err := writeCatalogEpoch(t.TempDir(), epoch{2026, 2}); err == nil {
		t.Fatal("expected error when catalog.toml does not exist")
	}
}

// ── test fixtures ─────────────────────────────────────────────────────────────

// stableCtx builds a cut-stable context wired to the given fakes.
func stableCtx(root string, g cutGit, gh cutGH, up releaseUploader) *cutContext {
	return &cutContext{
		root: root, channel: "stable",
		git: g, gh: gh, uploader: up,
		stdout: &strings.Builder{},
	}
}

// withYear pins currentYearFn for the test.
func withYear(t *testing.T, year int) {
	t.Helper()
	prev := currentYearFn
	currentYearFn = func() int { return year }
	t.Cleanup(func() { currentYearFn = prev })
}

func writeCatalogFile(t *testing.T, root, epochStr string) {
	t.Helper()
	body := "[catalog]\nepoch = \"" + epochStr + "\"\n"
	if err := os.WriteFile(filepath.Join(root, "catalog.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// depsHostedFixture builds a repo root with blobs.json seeded for the
// linux-amd64 (opt, llc) blobs the fake prebuilts.toml declares, plus a
// stubReleaseUploader that hosts (or, when hostAll is false, is missing) the
// corresponding <sha>.br assets on the deps-llvm-22.1.0 release.
func depsHostedFixture(t *testing.T, hostAll bool) (root string, shas map[string]string, up *stubReleaseUploader) {
	t.Helper()
	root, _ = fakeReleaseRoot(t, map[string]string{})
	shas = map[string]string{
		"opt": "1111111111111111111111111111111111111111111111111111111111111111",
		"llc": "2222222222222222222222222222222222222222222222222222222222222222",
	}
	cat := &BlobsCatalog{Schema: 1, Blobs: []BlobEntry{
		{Dependency: "llvm", Version: "22.1.0", Target: "linux-amd64", Name: "opt",
			SHA256: shas["opt"], Size: 100, Compression: compressionBrotli, CompressedSize: 50},
		{Dependency: "llvm", Version: "22.1.0", Target: "linux-amd64", Name: "llc",
			SHA256: shas["llc"], Size: 200, Compression: compressionBrotli, CompressedSize: 99},
	}}
	if err := WriteBlobsCatalog(root, cat); err != nil {
		t.Fatal(err)
	}
	up = newStubReleaseUploader()
	tag := DepsReleaseTag("llvm", "22.1.0")
	up.hosted[tag] = map[string]bool{}
	up.hosted[tag][shas["opt"]+".br"] = true
	if hostAll {
		up.hosted[tag][shas["llc"]+".br"] = true
	}
	return root, shas, up
}

// ── epoch parsing + small helpers ─────────────────────────────────────────────

func TestParseEpochStr(t *testing.T) {
	if e, err := parseEpochStr(" 2026.4 "); err != nil || e != (epoch{2026, 4}) {
		t.Fatalf("parseEpochStr = %v (%v), want 2026.4", e, err)
	}
	// Missing dot, non-numeric year, non-numeric number, empty, trailing segment.
	for _, bad := range []string{"2026", "20x6.1", "2026.x", "", "2026.1.2"} {
		if _, err := parseEpochStr(bad); err == nil {
			t.Errorf("parseEpochStr(%q) must error", bad)
		}
	}
}

func TestCutSmallHelpers(t *testing.T) {
	if short("abc") != "abc" {
		t.Fatal("short of a string <= 7 chars is unchanged")
	}
	if short("0123456789") != "0123456" {
		t.Fatal("short truncates to 7")
	}
	if sameSHA("", "abc") || sameSHA("abc", "") {
		t.Fatal("an empty SHA never matches")
	}
	if !sameSHA("abcdef123", "abcdef") || !sameSHA("abcdef", "abcdef123") {
		t.Fatal("short/full prefix forms must match either way")
	}
	if sameSHA("abcdef", "ghijkl") {
		t.Fatal("non-prefix SHAs must not match")
	}
	if lastStr(epoch{}, false) != "(none)" {
		t.Fatal("no last release → (none)")
	}
	if lastStr(epoch{2026, 1}, true) != "2026.1" {
		t.Fatal("last release → its string")
	}
}

func TestHighestReleasedEpochEdges(t *testing.T) {
	g := newFakeCutGit()
	// Defensive: tags without the `epoch-` prefix are skipped.
	g.epochTags = []string{"v1.0", "epoch-2026.1", "random"}
	last, ok, err := highestReleasedEpoch(g)
	if err != nil || !ok || last != (epoch{2026, 1}) {
		t.Fatalf("highest = %v ok=%v err=%v, want 2026.1", last, ok, err)
	}
	// A ListEpochTags tool failure propagates.
	g.listEpochErr = errors.New("git tag failed")
	if _, _, err := highestReleasedEpoch(g); err == nil {
		t.Fatal("ListEpochTags error must propagate")
	}
}

// ── confirmYearChange state machine ───────────────────────────────────────────

func TestConfirmYearChange(t *testing.T) {
	last, target := epoch{2026, 3}, epoch{2027, 0}
	if !confirmYearChange(&cutContext{confirmYear: true}, last, target) {
		t.Fatal("--confirm-year must authorize non-interactively")
	}
	if confirmYearChange(&cutContext{}, last, target) {
		t.Fatal("non-interactive without --confirm-year must decline")
	}
	yes := &cutContext{interactive: true, stdin: strings.NewReader("y\n"), stdout: &strings.Builder{}}
	if !confirmYearChange(yes, last, target) {
		t.Fatal("interactive 'y' must authorize")
	}
	no := &cutContext{interactive: true, stdin: strings.NewReader("n\n"), stdout: &strings.Builder{}}
	if confirmYearChange(no, last, target) {
		t.Fatal("interactive 'n' must decline")
	}
}

// ── gate 5: defensive epoch-rule re-check ─────────────────────────────────────

func TestGateEpochRuleValid(t *testing.T) {
	mk := func(target, last epoch, haveLast bool) gateResult {
		return gateEpochRuleValid(&cutContext{targetEpoch: target, lastEpoch: last, haveLast: haveLast})
	}
	if res := mk(epoch{2026, 0}, epoch{}, false); !res.passed {
		t.Fatalf("first release must pass: %s", res.detail)
	}
	if res := mk(epoch{2026, 4}, epoch{2026, 3}, true); !res.passed {
		t.Fatalf("same-year +1 must pass: %s", res.detail)
	}
	if res := mk(epoch{2027, 0}, epoch{2026, 3}, true); !res.passed {
		t.Fatalf("year jump to N=0 must pass: %s", res.detail)
	}
	// Each illegal transition fails overridably (an override cannot smuggle it
	// past derivation silently — but it stays auditable via --reason).
	for _, bad := range []struct{ target, last epoch }{
		{epoch{2026, 5}, epoch{2026, 3}}, // N skipped within a year
		{epoch{2027, 1}, epoch{2026, 3}}, // year jump but N>0
		{epoch{2026, 2}, epoch{2026, 3}}, // backward
	} {
		res := mk(bad.target, bad.last, true)
		if res.passed || !res.overridable {
			t.Fatalf("illegal %s after %s must fail overridably: %+v", bad.target, bad.last, res)
		}
	}
}

// ── catalog-sanity gate edges ─────────────────────────────────────────────────

func TestGateCatalogSanityUnreadable(t *testing.T) {
	root := t.TempDir() // no catalog.toml
	res := gateCatalogSanity(&cutContext{root: root})
	if !res.passed {
		t.Fatal("catalog sanity is informational for cut next and must never block")
	}
	if !strings.Contains(res.detail, "unreadable") {
		t.Fatalf("detail = %q, want an 'unreadable' note", res.detail)
	}
}

// ── preflight: a tool-error gate is a hard, non-bypassable fail ────────────────

func TestPreflightHardFailNotOverridable(t *testing.T) {
	g := newFakeCutGit()
	g.cleanErr = errors.New("git status exploded")
	ctx := baseCutCtx(&fakeCutGH{}, g)
	ctx.reason = "I insist" // --reason must NOT bypass a tool-error hard fail
	err := preflight(ctx, []gateFn{gateCleanTree})
	if err == nil {
		t.Fatal("a tool-error gate must abort preflight")
	}
	if !strings.Contains(err.Error(), "cannot be bypassed") {
		t.Fatalf("error = %v, want a non-bypassable abort", err)
	}
}

// ── ciStatusAtSHA edge cases + error propagation ──────────────────────────────

func TestCIStatusAtSHAErrors(t *testing.T) {
	sha := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if _, err := ciStatusAtSHA(&fakeCutGH{runsErr: errors.New("boom")}, sha); err == nil {
		t.Fatal("a WorkflowRuns error must propagate")
	}
	gh := greenCI(sha)
	gh.jobsErr = errors.New("run view failed")
	if _, err := ciStatusAtSHA(gh, sha); err == nil {
		t.Fatal("a RunJobs error must propagate")
	}
}

func TestCIStatusAtSHAIgnoresNonPlatformAndDuplicate(t *testing.T) {
	sha := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	gh := &fakeCutGH{
		// gh returns runs newest-first; run 1 decides linux (green) and carries a
		// non-platform "lint" job. Run 2 (older) re-reports linux as failure — it
		// must be ignored (already decided) — and supplies every other platform.
		ciRunsBefore: []ghRun{{DatabaseID: 1, HeadSHA: sha}, {DatabaseID: 2, HeadSHA: sha}},
		jobs: map[int64][]ghJob{
			1: {
				{Name: requiredPlatforms[0], Conclusion: "success"},
				{Name: "lint", Conclusion: "failure"},
			},
			2: olderRunReportingLinuxFailed(),
		},
	}
	status, err := ciStatusAtSHA(gh, sha)
	if err != nil {
		t.Fatal(err)
	}
	if status[requiredPlatforms[0]] != ciGreen {
		t.Fatalf("linux must stay green (newest run wins, older failure ignored): %v", status[requiredPlatforms[0]])
	}
	failed, absent := splitCIStatus(status)
	if len(failed) != 0 || len(absent) != 0 {
		t.Fatalf("non-platform 'lint' job must be ignored; failed=%v absent=%v", failed, absent)
	}
}

// ── gate 7: resolveAbsentCI failure branches ──────────────────────────────────

func TestGateToolErrorsAreHardFails(t *testing.T) {
	// The "a tool error is a non-overridable hard refusal" contract, across gates
	// other than gateCleanTree (covered by TestPreflightHardFailNotOverridable).
	g := newFakeCutGit()
	g.ancestorErr = errors.New("merge-base blew up")
	if res := gateReachable(&cutContext{git: g, targetSHA: g.head}); res.passed || res.overridable {
		t.Fatalf("gateReachable tool error must hard-fail: %+v", res)
	}
	// An unreadable catalog.toml (no file) makes gateCatalogEpoch a hard fail.
	res := gateCatalogEpoch(&cutContext{root: t.TempDir(), targetEpoch: epoch{2026, 1}})
	if res.passed || res.overridable {
		t.Fatalf("gateCatalogEpoch parse error must hard-fail: %+v", res)
	}
}

// TestGateCatalogEpochYearAdvance covers the year-advance exception (T0946): a
// prior-year catalog passes when yearAdvance is set, but a same-year mismatch or
// a catalog whose year is >= the target still fails overridably.
func TestGateCatalogEpochYearAdvance(t *testing.T) {
	mk := func(catalog string, target epoch, yearAdvance bool) gateResult {
		root := t.TempDir()
		writeCatalogFile(t, root, catalog)
		return gateCatalogEpoch(&cutContext{root: root, targetEpoch: target, yearAdvance: yearAdvance})
	}
	// Exact match always passes regardless of the flag.
	if res := mk("2027.0", epoch{2027, 0}, true); !res.passed {
		t.Fatalf("exact match must pass: %+v", res)
	}
	// Prior-year dev epoch + authorized year advance → accepted.
	res := mk("2026.4", epoch{2027, 0}, true)
	if !res.passed {
		t.Fatalf("prior-year catalog under year advance must pass: %+v", res)
	}
	if !strings.Contains(res.detail, "prior-year dev") {
		t.Fatalf("detail should note the prior-year acceptance, got: %q", res.detail)
	}
	// Same prior-year catalog WITHOUT the flag → overridable fail (no exception).
	if res := mk("2026.4", epoch{2027, 0}, false); res.passed || !res.overridable {
		t.Fatalf("prior-year catalog without year advance must fail overridably: %+v", res)
	}
	// Same-year mismatch → the exception must NOT apply, even with the flag set.
	if res := mk("2027.2", epoch{2027, 0}, true); res.passed || !res.overridable {
		t.Fatalf("same-year mismatch must still fail overridably: %+v", res)
	}
	// Catalog year AHEAD of the target → never accepted by the exception.
	if res := mk("2028.0", epoch{2027, 0}, true); res.passed || !res.overridable {
		t.Fatalf("catalog ahead of target must still fail overridably: %+v", res)
	}
}

func TestGateCIPerPlatformDispatchError(t *testing.T) {
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	// Some platforms already green, the rest absent → dispatchCI issues its single
	// run, and that dispatch fails.
	gh := greenCI(sha)
	gh.jobs[1] = gh.jobs[1][:2] // drop the trailing jobs → those platforms absent
	gh.dispatchErr = errors.New("gh down")
	ctx := baseCutCtx(gh, newFakeCutGit())
	ctx.runCI = true
	res := gateCI(ctx)
	if res.passed || res.overridable {
		t.Fatalf("a per-platform dispatch failure is a hard stop: %+v", res)
	}
	if !strings.Contains(res.detail, "dispatch ci.yml") {
		t.Fatalf("detail = %q", res.detail)
	}
}

func TestReadYes(t *testing.T) {
	if readYes(nil) {
		t.Fatal("a nil reader must be treated as 'no'")
	}
	if readYes(strings.NewReader("")) {
		t.Fatal("no input must be treated as 'no'")
	}
	for _, yes := range []string{"y\n", "Y", "yes\n", " YES "} {
		if !readYes(strings.NewReader(yes)) {
			t.Fatalf("readYes(%q) = false, want true", yes)
		}
	}
	for _, no := range []string{"n\n", "no", "nope", "maybe"} {
		if readYes(strings.NewReader(no)) {
			t.Fatalf("readYes(%q) = true, want false", no)
		}
	}
}

func TestGateCIQueryError(t *testing.T) {
	gh := &fakeCutGH{runsErr: errors.New("gh auth expired")}
	res := gateCI(baseCutCtx(gh, newFakeCutGit()))
	if res.passed || res.overridable {
		t.Fatalf("a CI query tool error must hard-fail: %+v", res)
	}
	if !strings.Contains(res.detail, "query CI status") {
		t.Fatalf("detail = %q", res.detail)
	}
}

func TestGateCIDispatchError(t *testing.T) {
	gh := &fakeCutGH{ciRunsBefore: nil, dispatchErr: errors.New("gh down")}
	ctx := baseCutCtx(gh, newFakeCutGit())
	ctx.runCI = true
	res := gateCI(ctx)
	if res.passed || res.overridable {
		t.Fatalf("a dispatch tool error is a hard stop: %+v", res)
	}
	if !strings.Contains(res.detail, "dispatch ci.yml") {
		t.Fatalf("detail = %q", res.detail)
	}
}

func TestGateCIFailedAfterDispatch(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	after := greenCI(sha)
	after.jobs[1][2] = ghJob{Name: requiredPlatforms[2], Status: "completed", Conclusion: "failure"}
	gh := &fakeCutGH{ciRunsBefore: nil, ciRunsAfter: after.ciRunsBefore, jobs: after.jobs}
	ctx := baseCutCtx(gh, newFakeCutGit())
	ctx.runCI = true
	res := gateCI(ctx)
	if res.passed {
		t.Fatal("a post-dispatch CI failure must not pass")
	}
	if !res.overridable {
		t.Fatal("a post-dispatch CI failure is overridable (a real red signal)")
	}
	if !strings.Contains(res.detail, "failed after dispatch") {
		t.Fatalf("detail = %q", res.detail)
	}
}

func TestGateCIStillPendingAfterWait(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	// Dispatch happens, but no run ever covers windows → windows stays absent →
	// watchCI exhausts its attempts and resolveAbsentCI reports a hard stop.
	after := greenCI(sha)
	after.jobs[1] = after.jobs[1][:2] // only linux + darwin ever complete
	gh := &fakeCutGH{ciRunsBefore: nil, ciRunsAfter: after.ciRunsBefore, jobs: after.jobs}
	ctx := baseCutCtx(gh, newFakeCutGit())
	ctx.runCI = true
	res := gateCI(ctx)
	if res.passed {
		t.Fatal("a still-absent CI after the wait must not pass")
	}
	if res.overridable {
		t.Fatal("still-pending after the wait is a hard stop, not overridable")
	}
	if !strings.Contains(res.detail, "still pending after wait") {
		t.Fatalf("detail = %q", res.detail)
	}
}

// ── cut stable: year rollover + multi-year-gap override ───────────────────────

func TestCutStableYearRolloverConfirmed(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	// A confirmed year rollover: catalog legitimately still holds the prior year's
	// dev epoch (no dev bump crosses the year boundary). gateCatalogEpoch accepts
	// it via the year-advance exception (T0946), and the post-cut bump advances
	// catalog to 2027.1.
	writeCatalogFile(t, root, "2026.4")

	g := newFakeCutGit()
	g.head = sha
	g.epochTags = []string{"epoch-2026.3"}
	g.tags["epoch-2026.3"] = "oldsha"
	g.tags["epoch-next"] = sha
	gh := greenCI(sha)
	gh.releaseRuns = []ghRun{{DatabaseID: 7, HeadSHA: sha, HeadBranch: "epoch-next", Conclusion: "success"}}

	ctx := stableCtx(root, g, gh, uploader)
	ctx.confirmYear = true // non-interactively confirm the year change
	withYear(t, 2027)
	if err := cutStable(ctx); err != nil {
		t.Fatalf("confirmed year-rollover cut stable failed: %v", err)
	}
	if len(g.createdTags) != 1 || g.createdTags[0].tag != "epoch-2027.0" {
		t.Fatalf("createdTags = %+v, want one epoch-2027.0", g.createdTags)
	}
	if got, _ := ParseEpoch(root); got != "2027.1" {
		t.Fatalf("catalog bump after rollover = %s, want 2027.1", got)
	}
	// The exception — not a --reason override — satisfied the catalog gate, so the
	// tag message must carry no "Gate override" audit line.
	if strings.Contains(g.createdTags[0].message, "Gate override") {
		t.Fatalf("confirmed rollover must not record a gate override; message: %q", g.createdTags[0].message)
	}
}

func TestCutStableYearRolloverNeedsConfirmation(t *testing.T) {
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.4") // prior-year dev epoch (realistic seed)
	g := newFakeCutGit()
	g.head = sha
	g.epochTags = []string{"epoch-2026.3"}
	g.tags["epoch-2026.3"] = "oldsha"
	ctx := stableCtx(root, g, greenCI(sha), uploader)
	// No --confirm-year, not interactive, no --reason → refuse before any tagging.
	withYear(t, 2027)
	err := cutStable(ctx)
	if err == nil {
		t.Fatal("a year rollover must refuse without confirmation")
	}
	if !strings.Contains(err.Error(), "requires confirmation") {
		t.Fatalf("error = %v, want a confirmation refusal", err)
	}
	if len(g.createdTags) != 0 {
		t.Fatal("an unconfirmed rollover must not tag")
	}
}

func TestCutStableMultiYearGapProceedsWithReason(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	// A prior-year dev epoch (not the Y.0 target): the year-advance exception must
	// carry the catalog gate on the gap path too, so the cut proceeds without the
	// catalog gate producing a second, redundant override.
	writeCatalogFile(t, root, "2024.1")

	g := newFakeCutGit()
	g.head = sha
	g.epochTags = []string{"epoch-2024.0"}
	g.tags["epoch-2024.0"] = "ancient"
	g.tags["epoch-next"] = sha
	gh := greenCI(sha)
	gh.releaseRuns = []ghRun{{DatabaseID: 7, HeadSHA: sha, HeadBranch: "epoch-next", Conclusion: "success"}}

	ctx := stableCtx(root, g, gh, uploader)
	ctx.reason = "multi-year hiatus; resuming releases"
	withYear(t, 2026)
	if err := cutStable(ctx); err != nil {
		t.Fatalf("multi-year gap with --reason should proceed: %v", err)
	}
	if len(g.createdTags) != 1 || g.createdTags[0].tag != "epoch-2026.0" {
		t.Fatalf("createdTags = %+v, want epoch-2026.0", g.createdTags)
	}
	if !strings.Contains(g.createdTags[0].message, "multi-year hiatus") {
		t.Fatalf("tag message must record the override reason, got: %q", g.createdTags[0].message)
	}
	// The reason is recorded once (for the derivation gap); the catalog gate is
	// satisfied by the year-advance exception, not a second override.
	if got := strings.Count(g.createdTags[0].message, "Gate override reason:"); got != 1 {
		t.Fatalf("tag message must record exactly one override reason, got %d: %q", got, g.createdTags[0].message)
	}
}

func TestCutStableFetchError(t *testing.T) {
	g := newFakeCutGit()
	g.fetchErr = errors.New("offline")
	ctx := stableCtx(t.TempDir(), g, &fakeCutGH{}, newStubReleaseUploader())
	if err := cutStable(ctx); err == nil || !strings.Contains(err.Error(), "git fetch") {
		t.Fatalf("a fetch error must abort cut stable: %v", err)
	}
	if len(g.createdTags) != 0 {
		t.Fatal("a failed fetch must not tag")
	}
}

// ── cut next: error paths ─────────────────────────────────────────────────────

func TestCutNextFetchError(t *testing.T) {
	g := newFakeCutGit()
	g.fetchErr = errors.New("no network")
	ctx := &cutContext{root: t.TempDir(), channel: "next", git: g, gh: &fakeCutGH{}, stdout: &strings.Builder{}}
	if err := cutNext(ctx); err == nil || !strings.Contains(err.Error(), "git fetch") {
		t.Fatalf("a fetch error must abort cut next: %v", err)
	}
}

func TestCutNextCreateTagError(t *testing.T) {
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")
	g := newFakeCutGit()
	g.head = sha
	g.createTagErr = errors.New("tag refused")
	ctx := &cutContext{root: root, channel: "next", git: g, gh: greenCI(sha), uploader: uploader, stdout: &strings.Builder{}}
	if err := cutNext(ctx); err == nil {
		t.Fatal("a CreateTag failure must abort cut next")
	}
	if len(g.pushedTags) != 0 {
		t.Fatal("must not push when the tag was not created")
	}
}

// ── gate 7: dispatch ref + concurrency (T1489) ───────────────────────────────

// cutDispatchCtx is an absent-CI context whose target is `sha`, ready to take
// the --run-ci dispatch path.
func cutDispatchCtx(t *testing.T, gh *fakeCutGH, g *fakeCutGit, sha string) *cutContext {
	t.Helper()
	ctx := baseCutCtx(gh, g)
	ctx.targetSHA = sha
	ctx.runCI = true
	return ctx
}

// TestCutRunCIDispatchesOnPinTagWhenPinned: a pinned cut must dispatch on its own
// immutable pin ref. Dispatching on the branch (the old behavior) tested whatever
// origin/main had moved to, so `cut --commit <past> --run-ci` could never produce
// CI at the pinned commit.
func TestCutRunCIDispatchesOnPinTagWhenPinned(t *testing.T) {
	noOpSleep(t)
	sha := "1111111111111111111111111111111111111111"
	g := newFakeCutGit()
	g.remotePinTags = []string{"ci-pin-stale"}
	gh := &fakeCutGH{
		ciRunsBefore: []ghRun{{DatabaseID: 3, HeadBranch: "ci-pin-stale", Status: "completed"}},
		ciRunsAfter:  greenCI(sha).ciRunsBefore,
		jobs:         greenCI(sha).jobs,
	}
	ctx := cutDispatchCtx(t, gh, g, sha)
	ctx.pinnedRef = sha
	if res := gateCI(ctx); !res.passed {
		t.Fatalf("pinned dispatch must pass: %s", res.detail)
	}
	wantTag := "ci-pin-" + short(sha)
	if len(g.pushedPinTags) != 1 || g.pushedPinTags[0] != wantTag {
		t.Fatalf("pushed pins = %v, want [%s]", g.pushedPinTags, wantTag)
	}
	if len(gh.dispatchedRefs) != 1 || gh.dispatchedRefs[0] != wantTag {
		t.Fatalf("dispatch refs = %v, want [%s] (never the branch)", gh.dispatchedRefs, wantTag)
	}
	// The stale pin is collected, and our own pin is removed once the watch ends.
	if strings.Join(g.deletedTags, ",") != "ci-pin-stale,"+wantTag {
		t.Errorf("deleted tags = %v, want [ci-pin-stale %s]", g.deletedTags, wantTag)
	}
}

// TestCutRunCINoCIWaitLeavesPin: with --no-ci-wait nothing tracks the run, so the
// pin must survive for the next dispatch's prune — deleting it here is exactly
// the T1489 bug.
func TestCutRunCINoCIWaitLeavesPin(t *testing.T) {
	sha := "1111111111111111111111111111111111111111"
	g := newFakeCutGit()
	gh := &fakeCutGH{}
	ctx := cutDispatchCtx(t, gh, g, sha)
	ctx.pinnedRef = sha
	ctx.noCIWait = true
	if res := gateCI(ctx); res.passed {
		t.Fatal("--no-ci-wait must not pass the gate")
	}
	if len(g.deletedTags) != 0 {
		t.Errorf("pin must outlive an unwatched dispatch, deleted %v", g.deletedTags)
	}
}

// TestCutRunCIPinSurvivesUnfinishedWatch: watchCI gives up after its attempt
// ceiling with platforms still pending. The run is still live, so deleting the
// ref it checks out is exactly T1489 — the pin must survive for a later prune.
func TestCutRunCIPinSurvivesUnfinishedWatch(t *testing.T) {
	noOpSleep(t)
	sha := "1111111111111111111111111111111111111111"
	g := newFakeCutGit()
	gh := &fakeCutGH{} // no runs ever appear → every platform stays absent
	ctx := cutDispatchCtx(t, gh, g, sha)
	ctx.pinnedRef = sha
	res := gateCI(ctx)
	if res.passed || !strings.Contains(res.detail, "still pending") {
		t.Fatalf("want a still-pending gate failure, got %+v", res)
	}
	if len(g.deletedTags) != 0 {
		t.Errorf("pin must outlive a run that never finished, deleted %v", g.deletedTags)
	}
}

// TestCutRunCIRefusesUnpushedPinCommit: gate 2 (reachable from origin/main) is
// overridable and every gate still runs after an override, so without this guard
// `--reason` would let the pin push a local-only commit to origin as a side
// effect of "run CI".
func TestCutRunCIRefusesUnpushedPinCommit(t *testing.T) {
	sha := "1111111111111111111111111111111111111111"
	g := newFakeCutGit()
	g.notOnOrigin = map[string]bool{sha: true}
	gh := &fakeCutGH{}
	ctx := cutDispatchCtx(t, gh, g, sha)
	ctx.pinnedRef = sha
	res := gateCI(ctx)
	if res.passed || !strings.Contains(res.detail, "is not on origin") {
		t.Fatalf("want a not-on-origin refusal, got %+v", res)
	}
	if len(g.pushedPinTags) != 0 || len(gh.dispatched) != 0 {
		t.Errorf("must not push a pin or dispatch: tags=%v dispatched=%v", g.pushedPinTags, gh.dispatched)
	}
}

// TestCutRunCINoCIWaitReportsJoinedRun: with --no-ci-wait the gate detail must
// say what actually happened — claiming a dispatch that never occurred (the run
// was already live at this SHA) misreports an outward-facing action.
func TestCutRunCINoCIWaitReportsJoinedRun(t *testing.T) {
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	gh := &fakeCutGH{ciRunsBefore: []ghRun{{DatabaseID: 4, HeadSHA: sha, HeadBranch: "main", Status: "in_progress"}}}
	ctx := cutDispatchCtx(t, gh, newFakeCutGit(), sha)
	ctx.noCIWait = true
	res := gateCI(ctx)
	if len(gh.dispatched) != 0 {
		t.Fatalf("must not dispatch over the run already testing this SHA, got %v", gh.dispatched)
	}
	if strings.Contains(res.detail, "dispatched ci.yml") {
		t.Errorf("detail must not claim a dispatch that did not happen: %q", res.detail)
	}
	if !strings.Contains(res.detail, "already running") {
		t.Errorf("detail = %q, want it to name the run it joined", res.detail)
	}
}

// TestCutRunCIUsesAllForMultipleAbsentPlatforms: several absent platforms become
// ONE platform=all run — a run per platform would self-cancel through ci.yml's
// shared concurrency group.
func TestCutRunCIUsesAllForMultipleAbsentPlatforms(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	before := greenCI(sha)
	before.jobs[1] = before.jobs[1][:len(requiredPlatforms)-2] // last two absent
	gh := &fakeCutGH{
		ciRunsBefore: before.ciRunsBefore,
		ciRunsAfter:  greenCI(sha).ciRunsBefore,
		jobs:         before.jobs,
	}
	ctx := cutDispatchCtx(t, gh, newFakeCutGit(), sha)
	res := gateCI(ctx)
	if len(gh.dispatched) != 1 {
		t.Fatalf("want exactly 1 dispatch for 2 absent platforms, got %v (%s)", gh.dispatched, res.detail)
	}
	if gh.dispatched[0]["platform"] != "all" {
		t.Errorf("platform = %q, want all", gh.dispatched[0]["platform"])
	}
}

// TestCutRunCIWaitsForMatchingInProgressRun: a live run on our dispatch ref at
// exactly the target SHA IS the run we want — wait on it, never cancel-and-requeue.
func TestCutRunCIWaitsForMatchingInProgressRun(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	// Run 4 is live at the target SHA. Its jobs are still pending on the gate's
	// first look (→ absent, so the dispatch path is entered) and green by the time
	// the watch polls.
	var jobCalls int
	gh := &fakeCutGH{
		ciRunsBefore: []ghRun{{DatabaseID: 4, HeadSHA: sha, HeadBranch: "main", Status: "in_progress"}},
		jobsFn: func(int64) ([]ghJob, error) {
			jobCalls++
			if jobCalls == 1 {
				return nil, nil
			}
			return greenCI(sha).jobs[1], nil
		},
	}
	ctx := cutDispatchCtx(t, gh, newFakeCutGit(), sha)
	res := gateCI(ctx)
	if !res.passed {
		t.Fatalf("must fall through to the watch and pass: %s", res.detail)
	}
	if len(gh.dispatched) != 0 {
		t.Errorf("must not dispatch over the run already testing this SHA, got %v", gh.dispatched)
	}
	if len(gh.cancelled) != 0 {
		t.Errorf("cut must never cancel a run, cancelled %v", gh.cancelled)
	}
}

// TestCutRunCIRejectsForeignInProgressRun: a live run on our ref at a DIFFERENT
// SHA would be silently cancelled by the dispatch — refuse, overridably.
func TestCutRunCIRejectsForeignInProgressRun(t *testing.T) {
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	gh := &fakeCutGH{
		ciRunsBefore: []ghRun{{DatabaseID: 4, HeadSHA: "ffffffffffffffffffffffffffffffffffffffff", HeadBranch: "main", Status: "in_progress"}},
	}
	ctx := cutDispatchCtx(t, gh, newFakeCutGit(), sha)
	res := gateCI(ctx)
	if res.passed {
		t.Fatal("a foreign live run must block the dispatch")
	}
	if !res.overridable {
		t.Error("waiting out a live run is an overridable gate, not a tool failure")
	}
	if !strings.Contains(res.detail, "#4") || !strings.Contains(res.detail, "gh run cancel 4") {
		t.Errorf("detail = %q, want the run id and a way out", res.detail)
	}
	// `cut` defines no --cancel-running flag, so pointing the reader at one would
	// send them into `flag provided but not defined`.
	if strings.Contains(res.detail, "--cancel-running") {
		t.Errorf("detail must not name a flag `cut` does not define: %q", res.detail)
	}
	if len(gh.dispatched) != 0 || len(gh.cancelled) != 0 {
		t.Errorf("must neither dispatch nor cancel: dispatched=%v cancelled=%v", gh.dispatched, gh.cancelled)
	}
}

// TestCutDispatchCIToolFailures: every tool call dispatchCI makes before the
// workflow_dispatch can fail, and each must abort the dispatch as a HARD gate
// failure (not overridable — only a live-run collision is "wait it out"), with
// no pin left behind for a run that will never exist.
func TestCutDispatchCIToolFailures(t *testing.T) {
	sha := "1111111111111111111111111111111111111111"
	cases := []struct {
		name   string
		pinned bool
		mutGit func(*fakeCutGit)
		mutGH  func(*fakeCutGH)
		want   string
	}{
		{
			// gateCI's own status query is call 1; dispatchCI's is call 2. Failing
			// exactly call 2 lands inside dispatchCI rather than short-circuiting
			// the gate before it ever gets there.
			name:  "run list fails",
			mutGH: func(gh *fakeCutGH) { gh.runsErrFrom = 2 },
			want:  "run list failed on call 2",
		},
		{
			name:   "current branch fails",
			mutGit: func(g *fakeCutGit) { g.branchErr = errors.New("detached") },
			want:   "current branch: detached",
		},
		{
			name:   "pin push fails",
			pinned: true,
			mutGit: func(g *fakeCutGit) { g.pushPinErr = errors.New("push rejected") },
			want:   "push rejected",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noOpSleep(t)
			g := newFakeCutGit()
			// A run exists at the SHA but reports no jobs, so every platform is
			// absent and the gate falls through to the dispatch path — while the
			// first WorkflowRuns query (gateCI's own) still succeeds.
			gh := &fakeCutGH{
				ciRunsBefore: []ghRun{{DatabaseID: 1, HeadSHA: sha, HeadBranch: "main", Status: "completed"}},
				jobs:         map[int64][]ghJob{1: {}},
			}
			if tc.mutGit != nil {
				tc.mutGit(g)
			}
			if tc.mutGH != nil {
				tc.mutGH(gh)
			}
			ctx := cutDispatchCtx(t, gh, g, sha)
			if tc.pinned {
				ctx.pinnedRef = sha
			}
			res := gateCI(ctx)
			if res.passed || !strings.Contains(res.detail, tc.want) {
				t.Fatalf("gate = %+v, want a failure mentioning %q", res, tc.want)
			}
			if res.overridable {
				t.Error("a tool failure is a hard stop, not an overridable gate")
			}
			if len(gh.dispatched) != 0 {
				t.Errorf("must not dispatch after the failure, got %v", gh.dispatched)
			}
			if len(g.pushedPinTags) != len(g.deletedTags) {
				t.Errorf("every pushed pin must be cleaned up: pushed=%v deleted=%v", g.pushedPinTags, g.deletedTags)
			}
		})
	}
}

// TestCutRunCIDispatchErrorCleansPin: a pin pushed for a dispatch that then
// fails is removed — no run will ever reference it, so leaving it would strand a
// ref the next prune cannot justify collecting (no completed run on it).
func TestCutRunCIDispatchErrorCleansPin(t *testing.T) {
	sha := "1111111111111111111111111111111111111111"
	g := newFakeCutGit()
	gh := &fakeCutGH{dispatchErr: errors.New("dispatch refused")}
	ctx := cutDispatchCtx(t, gh, g, sha)
	ctx.pinnedRef = sha
	res := gateCI(ctx)
	if res.passed || !strings.Contains(res.detail, "dispatch refused") {
		t.Fatalf("gate = %+v, want the dispatch error surfaced", res)
	}
	want := "ci-pin-" + short(sha)
	if strings.Join(g.pushedPinTags, ",") != want || strings.Join(g.deletedTags, ",") != want {
		t.Errorf("pin lifecycle = pushed %v / deleted %v, want both [%s]", g.pushedPinTags, g.deletedTags, want)
	}
}

// TestCutRunCIPinSurvivesWatchError: the watch's status query itself failing
// leaves the run's state UNKNOWN — it may still be queued, so deleting the ref it
// is about to check out is exactly T1489. The pin stays for a later prune.
func TestCutRunCIPinSurvivesWatchError(t *testing.T) {
	noOpSleep(t)
	sha := "1111111111111111111111111111111111111111"
	g := newFakeCutGit()
	gh := &fakeCutGH{runsErrAfter: errors.New("api down mid-watch")}
	ctx := cutDispatchCtx(t, gh, g, sha)
	ctx.pinnedRef = sha
	res := gateCI(ctx)
	if res.passed || !strings.Contains(res.detail, "watch CI: ") {
		t.Fatalf("gate = %+v, want a watch failure", res)
	}
	if !strings.Contains(res.detail, "api down mid-watch") {
		t.Errorf("detail = %q, want the underlying query error", res.detail)
	}
	if len(gh.dispatched) != 1 {
		t.Fatalf("the dispatch itself must have happened, got %v", gh.dispatched)
	}
	if len(g.deletedTags) != 0 {
		t.Errorf("run state is unknown — the pin must stay, deleted %v", g.deletedTags)
	}
}

// ── --commit flag (pin to an exact commit-ish) ───────────────────────────────

func TestCutNextWithCommitPin(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	pinnedSHA := "1111111111111111111111111111111111111111"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")

	g := newFakeCutGit()
	g.head = sha                      // current HEAD is different
	g.resolved[pinnedSHA] = pinnedSHA // the pinned --commit resolves to itself
	g.ancestorOK = true               // pinnedSHA is reachable
	g.epochTags = []string{"epoch-2026.0"}
	g.tags["epoch-2026.0"] = "oldsha"
	g.tags["epoch-next"] = pinnedSHA

	gh := greenCI(pinnedSHA)
	gh.releaseRuns = []ghRun{{DatabaseID: 7, HeadSHA: pinnedSHA, HeadBranch: "epoch-next", Conclusion: "success"}}

	ctx := &cutContext{
		root: root, channel: "next", pinnedRef: pinnedSHA,
		git: g, gh: gh, uploader: uploader, stdout: &strings.Builder{},
	}
	if err := cutNext(ctx); err != nil {
		t.Fatalf("cut next with --commit pin: %v", err)
	}
	if len(g.createdTags) != 1 || g.createdTags[0].tag != "epoch-next" {
		t.Fatalf("createdTags = %+v, want epoch-next", g.createdTags)
	}
	// Tag must be created at the PINNED sha, not HEAD
	if g.createdTags[0].sha != pinnedSHA {
		t.Fatalf("tagged sha = %s, want pinned SHA %s", g.createdTags[0].sha, pinnedSHA)
	}
}

func TestCutStableWithCommitPin(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	pinnedSHA := "2222222222222222222222222222222222222222"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")

	g := newFakeCutGit()
	g.head = sha                      // current HEAD is different
	g.resolved[pinnedSHA] = pinnedSHA // the pinned --commit resolves
	g.ancestorOK = true               // pinnedSHA is reachable from origin/main
	g.epochTags = []string{"epoch-2026.0"}
	g.tags["epoch-2026.0"] = "oldsha"
	g.tags["epoch-next"] = pinnedSHA

	gh := greenCI(pinnedSHA)
	gh.releaseRuns = []ghRun{{DatabaseID: 7, HeadSHA: pinnedSHA, HeadBranch: "epoch-next", Conclusion: "success"}}

	ctx := stableCtx(root, g, gh, uploader)
	ctx.pinnedRef = pinnedSHA // set the --commit pin
	withYear(t, 2026)
	if err := cutStable(ctx); err != nil {
		t.Fatalf("cut stable with --commit pin: %v", err)
	}
	if len(g.createdTags) != 1 || g.createdTags[0].tag != "epoch-2026.1" {
		t.Fatalf("createdTags = %+v, want epoch-2026.1", g.createdTags)
	}
	// Tag must be created at the PINNED sha, not HEAD
	if g.createdTags[0].sha != pinnedSHA {
		t.Fatalf("tagged sha = %s, want pinned SHA %s", g.createdTags[0].sha, pinnedSHA)
	}
}

func TestCutWithCommitResolveError(t *testing.T) {
	g := newFakeCutGit()
	g.resolveErr = errors.New("invalid ref")
	ctx := &cutContext{root: t.TempDir(), channel: "next", pinnedRef: "bad-ref", git: g, gh: &fakeCutGH{}, stdout: &strings.Builder{}}
	if err := cutNext(ctx); err == nil || !strings.Contains(err.Error(), "invalid ref") {
		t.Fatalf("ResolveSHA error must propagate: %v", err)
	}
}

func TestCutWithCommitRefPinning(t *testing.T) {
	// Test that a branch/tag ref (not a raw SHA) is resolved correctly.
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")

	g := newFakeCutGit()
	g.head = "ffffffffffffffffffffffffffffffffffffffff" // different HEAD
	g.resolved["release-branch"] = sha                  // the branch ref resolves to this sha
	g.ancestorOK = true                                 // sha is reachable
	g.epochTags = []string{"epoch-2026.0"}
	g.tags["epoch-2026.0"] = "oldsha"
	g.tags["epoch-next"] = sha

	gh := greenCI(sha)
	gh.releaseRuns = []ghRun{{DatabaseID: 7, HeadSHA: sha, HeadBranch: "epoch-next", Conclusion: "success"}}

	ctx := &cutContext{
		root: root, channel: "next", pinnedRef: "release-branch",
		git: g, gh: gh, uploader: uploader, stdout: &strings.Builder{},
	}
	if err := cutNext(ctx); err != nil {
		t.Fatalf("cut next with branch ref pin: %v", err)
	}
	if len(g.createdTags) != 1 || g.createdTags[0].sha != sha {
		t.Fatalf("tag must use resolved sha, got %+v", g.createdTags[0])
	}
}

// ── runReleaseCut public dispatch (swaps the production seam vars) ─────────────

func TestRunReleaseCutFlagParseError(t *testing.T) {
	if err := runReleaseCut(t.TempDir(), []string{"next", "--bogus"}); err == nil {
		t.Fatal("an unknown flag must surface a parse error")
	}
}

func TestRunReleaseCutDispatch(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")

	g := newFakeCutGit()
	g.head = sha
	gh := greenCI(sha)

	prevGit, prevGH, prevUp := defaultCutGit, defaultCutGH, defaultReleaseUploader
	defaultCutGit = func(string) cutGit { return g }
	defaultCutGH = gh
	defaultReleaseUploader = uploader
	t.Cleanup(func() {
		defaultCutGit, defaultCutGH, defaultReleaseUploader = prevGit, prevGH, prevUp
	})

	// "next" dispatch via the public entry — --dry-run leaves the tree untouched.
	if err := runReleaseCut(root, []string{"next", "--dry-run"}); err != nil {
		t.Fatalf("runReleaseCut next --dry-run: %v", err)
	}
	if len(g.createdTags) != 0 || len(g.pushedTags) != 0 {
		t.Fatalf("dry-run next mutated: tags=%v pushed=%v", g.createdTags, g.pushedTags)
	}

	// "stable" dispatch via the public entry — --dry-run runs every gate, mutates
	// nothing.
	g.epochTags = []string{"epoch-2026.0"}
	g.tags["epoch-2026.0"] = "old"
	g.tags["epoch-next"] = sha
	gh.releaseRuns = []ghRun{{DatabaseID: 7, HeadSHA: sha, HeadBranch: "epoch-next", Conclusion: "success"}}
	withYear(t, 2026)
	if err := runReleaseCut(root, []string{"stable", "--dry-run"}); err != nil {
		t.Fatalf("runReleaseCut stable --dry-run: %v", err)
	}
	if len(g.createdTags) != 0 || len(g.committed) != 0 {
		t.Fatalf("dry-run stable mutated: tags=%v commits=%v", g.createdTags, g.committed)
	}
}

// ── production seams: shellGit (temp repo) + shellGH (fake gh on PATH) ─────────

func TestShellGitSeam(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	remote := t.TempDir()
	work := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(remote, "init", "--bare")
	run(work, "init")
	run(work, "config", "user.name", "t")
	run(work, "config", "user.email", "t@t")
	run(work, "remote", "add", "origin", remote)
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644)
	run(work, "add", "a.txt")
	run(work, "commit", "-m", "first")
	run(work, "branch", "-M", "main")
	run(work, "push", "-u", "origin", "main")
	first := run(work, "rev-parse", "HEAD")

	g := shellGit{root: work}

	// CleanTree: clean, then dirty, then restored.
	if clean, err := g.CleanTree(); err != nil || !clean {
		t.Fatalf("CleanTree (clean) = %v (%v)", clean, err)
	}
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("dirty\n"), 0o644)
	if clean, err := g.CleanTree(); err != nil || clean {
		t.Fatalf("CleanTree (dirty) = %v (%v)", clean, err)
	}
	run(work, "checkout", "--", "a.txt")

	if sha, err := g.HeadSHA(); err != nil || sha != first {
		t.Fatalf("HeadSHA = %q (%v), want %s", sha, err, first)
	}
	if br, err := g.CurrentBranch(); err != nil || br != "main" {
		t.Fatalf("CurrentBranch = %q (%v), want main", br, err)
	}

	// A child commit, so we can exercise the ancestor relation both ways.
	os.WriteFile(filepath.Join(work, "b.txt"), []byte("b\n"), 0o644)
	run(work, "add", "b.txt")
	run(work, "commit", "-m", "second")
	second := run(work, "rev-parse", "HEAD")

	if anc, err := g.IsAncestor(first, second); err != nil || !anc {
		t.Fatalf("IsAncestor(first, second) = %v (%v), want true", anc, err)
	}
	if anc, err := g.IsAncestor(second, first); err != nil || anc {
		t.Fatalf("IsAncestor(second, first) = %v (%v), want false", anc, err)
	}

	// TagExists / TagSHA / CreateTag (annotated, no force) / ListEpochTags.
	if ex, err := g.TagExists("epoch-2099.0"); err != nil || ex {
		t.Fatalf("TagExists (absent) = %v (%v)", ex, err)
	}
	if err := g.CreateTag("epoch-2099.0", "msg", first, false); err != nil {
		t.Fatal(err)
	}
	if ex, err := g.TagExists("epoch-2099.0"); err != nil || !ex {
		t.Fatalf("TagExists (present) = %v (%v)", ex, err)
	}
	if sha, err := g.TagSHA("epoch-2099.0"); err != nil || sha != first {
		t.Fatalf("TagSHA = %q (%v), want %s", sha, err, first)
	}
	// Force-create the moving epoch-next at second, then re-point it to first.
	if err := g.CreateTag("epoch-next", "n", second, true); err != nil {
		t.Fatal(err)
	}
	if err := g.CreateTag("epoch-next", "n2", first, true); err != nil {
		t.Fatalf("force re-tag: %v", err)
	}
	if sha, _ := g.TagSHA("epoch-next"); sha != first {
		t.Fatalf("force re-tag sha = %s, want %s", sha, first)
	}
	tags, err := g.ListEpochTags()
	if err != nil {
		t.Fatal(err)
	}
	gotTags := strings.Join(tags, ",")
	if !strings.Contains(gotTags, "epoch-2099.0") || !strings.Contains(gotTags, "epoch-next") {
		t.Fatalf("ListEpochTags = %v, want both epoch-2099.0 and epoch-next", tags)
	}

	// ResolveSHA peels the annotated epoch-2099.0 tag (created at `first`) to its commit.
	if sha, err := g.ResolveSHA("epoch-2099.0"); err != nil || sha != first {
		t.Fatalf("ResolveSHA(epoch-2099.0) = %q (%v), want %s", sha, err, first)
	}
	// LogSubjects over first..second sees only the "second" commit; whole-history
	// (empty fromRef) sees both, newest first.
	if subs, err := g.LogSubjects(first, second); err != nil || len(subs) != 1 || subs[0] != "second" {
		t.Fatalf("LogSubjects(first..second) = %v (%v), want [second]", subs, err)
	}
	if subs, err := g.LogSubjects("", second); err != nil || len(subs) != 2 || subs[0] != "second" || subs[1] != "first" {
		t.Fatalf("LogSubjects(..second) = %v (%v), want [second first]", subs, err)
	}

	// Push tags/branch + fetch against the bare remote.
	if err := g.PushTag("epoch-2099.0", false); err != nil {
		t.Fatalf("PushTag: %v", err)
	}
	if err := g.PushTag("epoch-next", true); err != nil {
		t.Fatalf("PushTag (force): %v", err)
	}
	if err := g.PushBranch("main"); err != nil {
		t.Fatalf("PushBranch: %v", err)
	}
	if err := g.Fetch(); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// Pin refs: push at a sha → list → delete. `first` is on origin (pushed
	// above), so it is reachable; a commit that exists only locally is not.
	if reach, err := g.ReachableFromOrigin(first); err != nil || !reach {
		t.Fatalf("ReachableFromOrigin(pushed) = %v (%v), want true", reach, err)
	}
	run(work, "checkout", "-b", "local-only")
	os.WriteFile(filepath.Join(work, "d.txt"), []byte("d\n"), 0o644)
	run(work, "add", "d.txt")
	run(work, "commit", "-m", "unpushed")
	unpushed := run(work, "rev-parse", "HEAD")
	run(work, "checkout", "main")
	if reach, err := g.ReachableFromOrigin(unpushed); err != nil || reach {
		t.Fatalf("ReachableFromOrigin(unpushed) = %v (%v), want false", reach, err)
	}

	pin := "ci-pin-" + short(first)
	if err := g.PushTagAt(pin, first); err != nil {
		t.Fatalf("PushTagAt: %v", err)
	}
	pins, err := g.ListRemotePinTags()
	if err != nil {
		t.Fatalf("ListRemotePinTags: %v", err)
	}
	if strings.Join(pins, ",") != pin {
		t.Fatalf("ListRemotePinTags = %v, want [%s] (epoch tags must not match)", pins, pin)
	}
	if err := g.DeletePinTag(pin); err != nil {
		t.Fatalf("DeletePinTag: %v", err)
	}
	if pins, err := g.ListRemotePinTags(); err != nil || len(pins) != 0 {
		t.Fatalf("ListRemotePinTags after delete = %v (%v), want empty", pins, err)
	}

	// CommitFile stages + commits, leaving a clean tree.
	os.WriteFile(filepath.Join(work, "c.txt"), []byte("c\n"), 0o644)
	if err := g.CommitFile("c.txt", "add c"); err != nil {
		t.Fatalf("CommitFile: %v", err)
	}
	if clean, _ := g.CleanTree(); !clean {
		t.Fatal("tree must be clean after CommitFile")
	}
}

// newTempOriginRepo builds a work tree with a bare `origin` and one pushed
// commit on `main`, returning the shellGit under test, a `git` runner (dir +
// args), the work-tree path and the pushed HEAD sha.
func newTempOriginRepo(t *testing.T) (g shellGit, run func(dir string, args ...string) string, work, head string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	remote := t.TempDir()
	work = t.TempDir()
	run = func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(remote, "init", "--bare")
	run(work, "init")
	run(work, "config", "user.name", "t")
	run(work, "config", "user.email", "t@t")
	run(work, "remote", "add", "origin", remote)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "a.txt")
	run(work, "commit", "-m", "first")
	run(work, "branch", "-M", "main")
	run(work, "push", "-u", "origin", "main")
	return shellGit{root: work}, run, work, run(work, "rev-parse", "HEAD")
}

// TestShellGitRemoteBranchSHA covers the query behind `ci`'s HEAD-is-remote-tip
// guard: workflow_dispatch checks out origin's tip, so a wrong answer here either
// blocks a legitimate dispatch or lets `ci` claim it tested a commit it did not.
func TestShellGitRemoteBranchSHA(t *testing.T) {
	g, run, work, head := newTempOriginRepo(t)

	if sha, err := g.RemoteBranchSHA("main"); err != nil || sha != head {
		t.Fatalf("RemoteBranchSHA(main) = %q (%v), want %s", sha, err, head)
	}
	// Absent on origin is "" with NO error — `ci` turns that into "push it first",
	// which an error would instead render as a git malfunction.
	if sha, err := g.RemoteBranchSHA("no-such-branch"); err != nil || sha != "" {
		t.Fatalf("RemoteBranchSHA(absent) = %q (%v), want \"\"", sha, err)
	}
	// A tag-only match resolves to the tag's tip.
	run(work, "tag", "release-marker", head)
	run(work, "push", "origin", "refs/tags/release-marker")
	if sha, err := g.RemoteBranchSHA("release-marker"); err != nil || sha != head {
		t.Fatalf("RemoteBranchSHA(tag) = %q (%v), want %s", sha, err, head)
	}
	// When a branch and a tag share a name, the BRANCH wins — `ci` dispatches on a
	// branch ref, so comparing HEAD against the tag's commit would guard the wrong
	// thing. Point them at different commits so the preference is observable.
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "b.txt")
	run(work, "commit", "-m", "second")
	second := run(work, "rev-parse", "HEAD")
	run(work, "branch", "shared", second)
	run(work, "push", "origin", "shared")
	run(work, "tag", "shared", head)
	run(work, "push", "origin", "refs/tags/shared")
	if sha, err := g.RemoteBranchSHA("shared"); err != nil || sha != second {
		t.Fatalf("RemoteBranchSHA(shared) = %q (%v), want the branch tip %s not the tag %s", sha, err, second, head)
	}

	run(work, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
	if sha, err := g.RemoteBranchSHA("main"); err == nil {
		t.Errorf("RemoteBranchSHA = %q, want an error when origin is unreachable", sha)
	}
}

// TestShellGitPinRefEdges covers the pin-ref seam's edges, which the happy path
// in TestShellGitSeam does not reach: annotated tags (whose `ls-remote` output
// names each tag TWICE), local-copy cleanup, and each call's failure mode.
func TestShellGitPinRefEdges(t *testing.T) {
	g, run, work, head := newTempOriginRepo(t)

	// An ANNOTATED pin: `git ls-remote --tags` lists the tag object AND its
	// peeled `…^{}` commit, so a naive parse would report the same pin twice and
	// try to delete it twice (the second delete failing loudly).
	annotated := "ci-pin-annotated"
	run(work, "tag", "-a", annotated, "-m", "annotated pin", head)
	run(work, "push", "origin", "refs/tags/"+annotated)
	pins, err := g.ListRemotePinTags()
	if err != nil {
		t.Fatalf("ListRemotePinTags: %v", err)
	}
	if strings.Join(pins, ",") != annotated {
		t.Fatalf("ListRemotePinTags = %v, want exactly [%s] (peeled entries deduped)", pins, annotated)
	}

	// DeletePinTag also drops the LOCAL copy: `git fetch origin --tags` leaves one
	// behind and fetch never prunes tags, so a survivor would be re-pushed by the
	// next `git push --tags`, resurrecting a pin nothing tracks.
	if err := g.DeletePinTag(annotated); err != nil {
		t.Fatalf("DeletePinTag: %v", err)
	}
	if out := run(work, "tag", "--list", annotated); out != "" {
		t.Errorf("local tag %s survived DeletePinTag: %q", annotated, out)
	}
	if pins, err := g.ListRemotePinTags(); err != nil || len(pins) != 0 {
		t.Fatalf("ListRemotePinTags after delete = %v (%v), want empty", pins, err)
	}

	// Deleting a pin origin no longer has is IDEMPOTENT (git only warns), which is
	// what the prune relies on: two invocations can race on the same stale pin and
	// the loser must not turn a harmless double-delete into a user-facing warning.
	if err := g.DeletePinTag(annotated); err != nil {
		t.Errorf("re-deleting an absent pin must be a no-op, got %v", err)
	}

	// ReachableFromOrigin on a name git cannot resolve is an ERROR, not "false":
	// silently reporting "not reachable" would turn a git malfunction into the
	// user-facing "push it first" advice.
	if _, err := g.ReachableFromOrigin("not-a-real-object"); err == nil {
		t.Error("ReachableFromOrigin must surface a malformed object name")
	}

	// A broken origin makes every remote-touching call fail, and ListRemotePinTags
	// must propagate it rather than report "no pins" (which would silently disable
	// pruning forever).
	run(work, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
	if pins, err := g.ListRemotePinTags(); err == nil {
		t.Errorf("ListRemotePinTags = %v, want an error when origin is unreachable", pins)
	}
	if err := g.PushTagAt("ci-pin-x", head); err == nil {
		t.Error("PushTagAt must surface an unreachable origin")
	}
	if err := g.DeletePinTag("ci-pin-x"); err == nil {
		t.Error("DeletePinTag must surface an unreachable origin")
	}
}

// writeFakeGH drops a fake `gh` shell script onto PATH for the duration of a test.
// writeFakeGH drops a shell-script `gh` stub into a fresh temp dir and prepends
// that dir to PATH so shellGH/ghCLIUploader exec it instead of a real gh. The
// mechanism — an extensionless shell script resolved as `gh` by exec.LookPath —
// is Unix-only: Windows resolves executables by PATHEXT extension, so a bare
// `gh` script can never stand in for the tool there (LookPath skips it and falls
// through to a real gh.exe, or fails). Tests that rely on it are skipped on
// Windows; the shellGH JSON/arg logic they cover is platform-independent and
// stays exercised on the Linux/macOS runners (T1225).
func writeFakeGH(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake gh shell-script stub is not executable as `gh` on Windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestShellGHParse(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	writeFakeGH(t, `#!/bin/sh
case "$1 $2" in
  "run list") echo '[{"databaseId":42,"headSha":"abc","headBranch":"main","status":"completed","conclusion":"success"}]';;
  "run view") echo '{"jobs":[{"name":"linux-amd64","status":"completed","conclusion":"success"}]}';;
  *) exit 0;;
esac
`)
	runs, err := (shellGH{}).WorkflowRuns("ci.yml", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].DatabaseID != 42 || runs[0].Conclusion != "success" {
		t.Fatalf("WorkflowRuns = %+v", runs)
	}
	jobs, err := (shellGH{}).RunJobs(42)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Name != "linux-amd64" {
		t.Fatalf("RunJobs = %+v", jobs)
	}
}

func TestShellGHDispatchArgs(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	out := filepath.Join(t.TempDir(), "argv")
	writeFakeGH(t, "#!/bin/sh\necho \"$@\" > \""+out+"\"\nexit 0\n")
	if err := (shellGH{}).DispatchWorkflow("ci.yml", "main", map[string]string{"platform": "all", "run_tests": "true"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	// Inputs are emitted in sorted key order (platform before run_tests).
	want := "workflow run ci.yml --ref main -f platform=all -f run_tests=true"
	if line := strings.TrimSpace(string(got)); line != want {
		t.Fatalf("dispatch argv = %q, want %q", line, want)
	}
}

// TestShellGHCancelRunArgs pins the argv `gh run cancel` is invoked with — the
// run ID is formatted as a decimal string, so a large real-world ID (they are
// well past 2^31) must not be truncated or rendered in scientific notation.
func TestShellGHCancelRunArgs(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	out := filepath.Join(t.TempDir(), "argv")
	writeFakeGH(t, "#!/bin/sh\necho \"$@\" > \""+out+"\"\nexit 0\n")
	if err := (shellGH{}).CancelRun(31660226927); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if line := strings.TrimSpace(string(got)); line != "run cancel 31660226927" {
		t.Fatalf("cancel argv = %q, want %q", line, "run cancel 31660226927")
	}
}

func TestShellGHErrors(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	// gh exits non-zero → all three calls surface the failure.
	writeFakeGH(t, "#!/bin/sh\nexit 3\n")
	if _, err := (shellGH{}).WorkflowRuns("ci.yml", 5); err == nil {
		t.Fatal("WorkflowRuns must surface a gh failure")
	}
	if _, err := (shellGH{}).RunJobs(1); err == nil {
		t.Fatal("RunJobs must surface a gh failure")
	}
	if err := (shellGH{}).DispatchWorkflow("ci.yml", "main", nil); err == nil {
		t.Fatal("DispatchWorkflow must surface a gh failure")
	}
	// A failed cancel MUST surface: --cancel-running dispatches only if the
	// cancel succeeded, so swallowing this would silently cancel the run anyway
	// through ci.yml's concurrency group — the exact behavior the guard prevents.
	if err := (shellGH{}).CancelRun(42); err == nil {
		t.Fatal("CancelRun must surface a gh failure")
	}
	// gh exits 0 but emits non-JSON → the parse errors surface.
	writeFakeGH(t, "#!/bin/sh\necho 'not json'\nexit 0\n")
	if _, err := (shellGH{}).WorkflowRuns("ci.yml", 5); err == nil {
		t.Fatal("WorkflowRuns must surface a JSON parse error")
	}
	if _, err := (shellGH{}).RunJobs(1); err == nil {
		t.Fatal("RunJobs must surface a JSON parse error")
	}
}

// ── resolveNotesBody ─────────────────────────────────────────────────────────

func TestResolveNotesBodyDefault(t *testing.T) {
	g := newFakeCutGit()
	g.logSubjects = []string{"fix: something"}
	ctx := &cutContext{git: g, targetSHA: "deadbeef", lastEpoch: epoch{2026, 0}, haveLast: true}
	got, err := resolveNotesBody(ctx)
	if err != nil {
		t.Fatalf("resolveNotesBody (default): %v", err)
	}
	want, _ := generateReleaseNotes(ctx)
	if got != want {
		t.Fatalf("default path must equal generateReleaseNotes:\ngot:  %q\nwant: %q", got, want)
	}
	if !strings.Contains(got, installHeader) {
		t.Fatalf("default notes must contain install header")
	}
}

func TestResolveNotesBodyInline(t *testing.T) {
	ctx := &cutContext{notesInline: "hand-written notes"}
	got, err := resolveNotesBody(ctx)
	if err != nil {
		t.Fatalf("resolveNotesBody (inline): %v", err)
	}
	want := installHeader + "hand-written notes\n"
	if got != want {
		t.Fatalf("inline notes = %q, want %q", got, want)
	}
}

func TestResolveNotesBodyFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(f, []byte("file body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := &cutContext{notesFile: f}
	got, err := resolveNotesBody(ctx)
	if err != nil {
		t.Fatalf("resolveNotesBody (file): %v", err)
	}
	want := installHeader + "file body\n"
	if got != want {
		t.Fatalf("file notes = %q, want %q", got, want)
	}
}

func TestResolveNotesBodyStdin(t *testing.T) {
	ctx := &cutContext{
		notesFile: "-",
		stdin:     strings.NewReader("stdin notes\n"),
	}
	got, err := resolveNotesBody(ctx)
	if err != nil {
		t.Fatalf("resolveNotesBody (stdin): %v", err)
	}
	want := installHeader + "stdin notes\n"
	if got != want {
		t.Fatalf("stdin notes = %q, want %q", got, want)
	}
}

func TestResolveNotesBodyFileMissing(t *testing.T) {
	ctx := &cutContext{notesFile: filepath.Join(t.TempDir(), "nonexistent.md")}
	if _, err := resolveNotesBody(ctx); err == nil {
		t.Fatal("a missing notes file must return an error")
	}
}

func TestRunReleaseCutMutualExclusion(t *testing.T) {
	err := runReleaseCut(t.TempDir(), []string{"next", "--notes-file", "f.md", "--notes", "x"})
	if err == nil {
		t.Fatal("--notes-file and --notes together must error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v, want 'mutually exclusive'", err)
	}
}

func TestCutNextDryRunCustomNotes(t *testing.T) {
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")
	g := newFakeCutGit()
	g.head = sha
	out := &strings.Builder{}
	ctx := &cutContext{
		root: root, channel: "next", dryRun: true,
		notesInline: "hand-written notes",
		git:         g, gh: greenCI(sha), uploader: uploader,
		stdin: strings.NewReader(""), stdout: out,
	}
	if err := cutNext(ctx); err != nil {
		t.Fatalf("cut next --dry-run with custom notes: %v", err)
	}
	if len(g.createdTags) != 0 || len(g.pushedTags) != 0 {
		t.Fatal("--dry-run must not mutate")
	}
	printed := out.String()
	if !strings.Contains(printed, "hand-written notes") {
		t.Fatalf("dry-run output must contain custom notes body, got:\n%s", printed)
	}
	if !strings.Contains(printed, installHeader) {
		t.Fatalf("dry-run output must contain install header, got:\n%s", printed)
	}
}

func TestCutStableDryRunCustomNotes(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")

	g := newFakeCutGit()
	g.head = sha
	g.epochTags = []string{"epoch-2026.0"}
	g.tags["epoch-2026.0"] = "oldsha"
	g.tags["epoch-next"] = sha
	gh := greenCI(sha)
	gh.releaseRuns = []ghRun{{DatabaseID: 7, HeadSHA: sha, HeadBranch: "epoch-next", Conclusion: "success"}}

	out := &strings.Builder{}
	ctx := stableCtx(root, g, gh, uploader)
	ctx.dryRun = true
	ctx.notesInline = "AI-crafted summary"
	ctx.stdout = out
	withYear(t, 2026)
	if err := cutStable(ctx); err != nil {
		t.Fatalf("cut stable --dry-run with custom notes: %v", err)
	}
	if len(g.createdTags) != 0 || len(g.committed) != 0 {
		t.Fatal("--dry-run must not mutate")
	}
	printed := out.String()
	if !strings.Contains(printed, "AI-crafted summary") {
		t.Fatalf("dry-run output must contain custom notes body, got:\n%s", printed)
	}
	if !strings.Contains(printed, installHeader) {
		t.Fatalf("dry-run output must contain install header, got:\n%s", printed)
	}
}

// failReader is an io.Reader that always returns an error.
type failReader struct{ err error }

func (r *failReader) Read(p []byte) (int, error) { return 0, r.err }

// TestReadNotesFileStdinError covers the io.ReadAll error branch in readNotesFile
// (the one uncovered path in the function).
func TestReadNotesFileStdinError(t *testing.T) {
	ctx := &cutContext{
		notesFile: "-",
		stdin:     &failReader{err: errors.New("stdin pipe broken")},
	}
	if _, err := resolveNotesBody(ctx); err == nil {
		t.Fatal("a stdin read error must propagate out of resolveNotesBody")
	}
}

// TestCutNextCustomNotesInTagMessage verifies that custom notes (--notes) flow all
// the way into the annotated tag message on a real (non-dry-run) cut next.
func TestCutNextCustomNotesInTagMessage(t *testing.T) {
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")
	g := newFakeCutGit()
	g.head = sha
	ctx := &cutContext{
		root: root, channel: "next",
		notesInline: "custom AI summary",
		git:         g, gh: greenCI(sha), uploader: uploader,
		stdout: &strings.Builder{},
	}
	if err := cutNext(ctx); err != nil {
		t.Fatalf("cut next with custom notes failed: %v", err)
	}
	if len(g.createdTags) != 1 {
		t.Fatalf("expected one created tag, got %d", len(g.createdTags))
	}
	msg := g.createdTags[0].message
	if !strings.Contains(msg, "custom AI summary") {
		t.Fatalf("tag message must contain custom notes body, got: %q", msg)
	}
	if !strings.Contains(msg, installHeader) {
		t.Fatalf("tag message must contain install header, got: %q", msg)
	}
}

// TestCutStableCustomNotesInTagMessage verifies that custom notes (--notes) flow
// all the way into the annotated tag message on a real (non-dry-run) cut stable.
func TestCutStableCustomNotesInTagMessage(t *testing.T) {
	noOpSleep(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	root, _, uploader := depsHostedFixture(t, true)
	writeCatalogFile(t, root, "2026.1")

	g := newFakeCutGit()
	g.head = sha
	g.epochTags = []string{"epoch-2026.0"}
	g.tags["epoch-2026.0"] = "oldsha"
	g.tags["epoch-next"] = sha

	gh := greenCI(sha)
	gh.releaseRuns = []ghRun{{DatabaseID: 7, HeadSHA: sha, HeadBranch: "epoch-next", Conclusion: "success"}}

	ctx := stableCtx(root, g, gh, uploader)
	ctx.notesInline = "hand-crafted release summary"
	withYear(t, 2026)
	if err := cutStable(ctx); err != nil {
		t.Fatalf("cut stable with custom notes failed: %v", err)
	}
	if len(g.createdTags) != 1 {
		t.Fatalf("expected one created tag, got %d", len(g.createdTags))
	}
	msg := g.createdTags[0].message
	if !strings.Contains(msg, "hand-crafted release summary") {
		t.Fatalf("tag message must contain custom notes body, got: %q", msg)
	}
	if !strings.Contains(msg, installHeader) {
		t.Fatalf("tag message must contain install header, got: %q", msg)
	}
}
