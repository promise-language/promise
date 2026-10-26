package common

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/promise/tools/build/internal/hostscope"
)

// TestRunVerify_UsageErrorsWriteNothing: verify takes no flag of its own, so
// every flag outside the built-ins — a typo, or one the tool contract retired —
// is a usage error (exit 2) found before any lock or filesystem side effect,
// with nothing on stdout. A retired flag is told apart from a typo by nothing
// but its name: neither changes what is measured, and both are refused.
func TestRunVerify_UsageErrorsWriteNothing(t *testing.T) {
	cliObserved(t, Observed()) // RunVerify records whether anyone watches
	for _, argv := range [][]string{{"--unknown"}, {"--clean"}, {"--push"}, {"--lock-timeout=10m"}, {"--wasm"}, {"stray"}, {"-json", "-human"}} {
		var out bytes.Buffer
		err := RunVerify(t.TempDir(), argv, &out)
		if got := ExitCode(err); got != 2 {
			t.Errorf("%v: exit = %d (%v), want 2", argv, got, err)
		}
		if out.Len() != 0 {
			t.Errorf("%v: a usage error wrote %q on stdout", argv, out.String())
		}
	}
}

// TestRunVerify_HelpAndVersionAnswerWithoutRunning: -help names no command and
// -version the stamped hash, and neither takes the lock or builds anything.
func TestRunVerify_HelpAndVersionAnswerWithoutRunning(t *testing.T) {
	var out bytes.Buffer
	if err := RunVerify("/nowhere", []string{"-help", "-json"}, &out); err != nil {
		t.Fatal(err)
	}
	var help struct {
		Project  string            `json:"project"`
		Commands []json.RawMessage `json:"commands"`
	}
	if err := json.Unmarshal(out.Bytes(), &help); err != nil || help.Project != "promise" || help.Commands == nil || len(help.Commands) != 0 {
		t.Errorf("-help -json = %s (%v), want project promise and \"commands\": []", out.String(), err)
	}
	out.Reset()
	if err := RunVerify("/nowhere", []string{"-version", "-json"}, &out); err != nil || !strings.HasPrefix(out.String(), `{"project":"promise","text":`) {
		t.Errorf("-version -json = %q, %v", out.String(), err)
	}
}

// errQueued is acquireWithin's answer for an exclusion not granted in time.
var errQueued = errors.New("queued past the bound")

// acquireWithin takes the host scope for root as acquireHostScope does, giving
// up after d (0 waits as long as it takes). Asking with a bound is how a test
// asks "would this queue?" of an acquire that, in production, never gives up.
func acquireWithin(root string, d time.Duration) (func(), error) {
	ctx := context.Background()
	if d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	release, err := acquireHostScopeCtx(ctx, root)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, errQueued
	}
	return release, err
}

// arenaRoot is a checkout this test owns, provisioned as the arena id names:
// the same .workspace/arena.json a flow runner reads to name its holder.
func arenaRoot(t *testing.T, id string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, ".workspace/arena.json", `{"id":"`+id+`","label":"test"}`+"\n")
	return root
}

const (
	testArenaID = "a-00000000000000000000000000000001"
	peerArenaID = "a-00000000000000000000000000000002"
	testHostID  = "h-00000000000000000000000000000001"
)

// provisionHost writes the host identity record workspace setup writes, into
// the host home under the home cleanTestHome redirected: the record a flow
// runner reads the host half of its arena from.
func provisionHost(t *testing.T, content string) {
	t.Helper()
	hostHome, ok := hostscope.HostHome()
	if !ok {
		t.Fatal("hostscope.HostHome: no host home under the redirected home")
	}
	writeFile(t, hostHome, "identity.json", content)
}

// provisionedHost is the record of the machine the test arenas are on.
const provisionedHost = `{"kind":"host","id":"` + testHostID + `","label":"test"}`

// TestAcquireHostScope_NamesThisCheckoutsArena: the record a refused party
// reads names the arena this checkout is, so a runner of the same arena can
// recognise it — and nothing else, since the arena is what the exclusion is
// re-entered on.
func TestAcquireHostScope_NamesThisCheckoutsArena(t *testing.T) {
	cleanTestHome(t)
	provisionHost(t, provisionedHost)
	root := arenaRoot(t, testArenaID)

	unlock, err := acquireWithin(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	got, ok := hostscope.Holder()
	want := hostscope.ArenaIdentity{Host: testHostID, Id: testArenaID}
	if !ok || got != want {
		t.Errorf("the exclusion names %v (%v), want this checkout's arena %v", got, ok, want)
	}
}

// TestAcquireHostScope_UnboundedAcquiresAfterRelease confirms lockTimeout=0
// waits (does not time out) and succeeds once the holder releases. The waiter
// has no arena record — a contributor clone — and still queues like any party.
func TestAcquireHostScope_UnboundedAcquiresAfterRelease(t *testing.T) {
	cleanTestHome(t)
	unlock, err := acquireWithin(arenaRoot(t, peerArenaID), 0)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan error, 1)
	go func() {
		inner, ierr := acquireWithin(t.TempDir(), 0)
		if ierr == nil {
			inner()
		}
		acquired <- ierr
	}()

	select {
	case ierr := <-acquired:
		unlock()
		t.Fatalf("the waiter was answered (%v) while a peer held the exclusion", ierr)
	case <-time.After(100 * time.Millisecond):
		// Still waiting, as it must be.
	}
	unlock()

	select {
	case ierr := <-acquired:
		if ierr != nil {
			t.Fatalf("unbounded waiter failed: %v", ierr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unbounded waiter did not acquire the lock after release")
	}
}

// TestAcquireHostScope_LetsTheHoldingArenaStraightThrough is the runner case
// (#96): a flow runner holds the exclusion for this checkout's arena and spawns
// bin/verify, which must be let straight through rather than queue behind its
// own parent inside the run's allowance. Its release must not hand the machine
// to a peer while the runner still holds it.
//
// The runner's holder is the pair flow.ArenaAt reads from the machine's host
// record and the checkout's arena record, written here as those records' ids —
// never as hostscope.ArenaAt's answer, which would derive both sides the same
// way and pass however verify named itself (#642: the copy derived the host
// from the host name, and every runner's verify queued behind its parent).
func TestAcquireHostScope_LetsTheHoldingArenaStraightThrough(t *testing.T) {
	cleanTestHome(t)
	provisionHost(t, provisionedHost)
	root := arenaRoot(t, testArenaID)
	// The runner's acquisition: flow takes it as hostscope.Acquire(ctx,
	// flow.ArenaAt(hostHome, worktree)).
	runner, _, err := hostscope.Acquire(context.Background(), hostscope.ArenaIdentity{Host: testHostID, Id: testArenaID})
	if err != nil {
		t.Fatal(err)
	}
	defer runner()

	nested, err := acquireWithin(root, 5*time.Second)
	if err != nil {
		t.Fatalf("a verify in the holding arena could not enter the exclusion its arena holds: %v", err)
	}
	nested()

	if _, err := acquireWithin(arenaRoot(t, peerArenaID), 150*time.Millisecond); !errors.Is(err, errQueued) {
		t.Fatalf("a peer was not excluded after the nested release (err = %v); the runner still holds it", err)
	}
}

// TestAcquireHostScope_AHandRunVerifyHoldsOffARunner is the other direction of
// #96: a verify run by hand in one checkout holds the exclusion, and a runner of
// another arena — taking it exactly as flow does, before it spawns its own verify
// — queues behind it and is granted when it ends, with the queue reported as a
// wait (what the SDK files as CommandRun.Waited, outside the allowance). The
// defect this replaced was a hand-run verify holding a lock of its own that the
// runner's acquisition never saw.
func TestAcquireHostScope_AHandRunVerifyHoldsOffARunner(t *testing.T) {
	cleanTestHome(t)
	provisionHost(t, provisionedHost)
	hand, err := acquireWithin(arenaRoot(t, peerArenaID), 0)
	if err != nil {
		t.Fatal(err)
	}
	// The runner of another arena on this machine, named as flow names it.
	runnerArena := hostscope.ArenaIdentity{Host: testHostID, Id: testArenaID}

	queued := make(chan struct{}, 1)
	ctx := hostscope.OnQueue(context.Background(), func(hostscope.Scope) func() {
		queued <- struct{}{}
		return nil
	})
	type result struct {
		release func()
		waited  time.Duration
		err     error
	}
	done := make(chan result, 1)
	go func() {
		release, waited, err := hostscope.Acquire(ctx, runnerArena)
		done <- result{release, waited, err}
	}()

	select {
	case <-queued:
		// Refused by the kernel and not re-entered: it is behind the hand run.
	case r := <-done:
		hand()
		if r.err == nil {
			r.release()
		}
		t.Fatalf("the runner was answered (%v) while a hand-run verify held the exclusion", r.err)
	case <-time.After(10 * time.Second):
		hand()
		t.Fatal("the runner never queued behind the hand-run verify")
	}
	hand()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("the runner was not granted the exclusion after the hand-run verify ended: %v", r.err)
		}
		r.release()
		if r.waited <= 0 {
			t.Errorf("waited = %s, but the runner queued behind the hand-run verify", r.waited)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the runner was not granted the exclusion within 10s of the hand-run verify ending")
	}
}

// TestAcquireHostScope_CheckoutsWithNoArenaRecordExcludeEachOther: a checkout no
// flow provisioned (a contributor clone, a CI runner) has no arena to re-enter
// on, so it is a party like any other — two of them exclude each other, and so
// does a second acquisition from the same one, which is why `verify --clean`
// calls cleanLocked rather than Clean.
func TestAcquireHostScope_CheckoutsWithNoArenaRecordExcludeEachOther(t *testing.T) {
	cleanTestHome(t)
	root := t.TempDir()
	unlock, err := acquireWithin(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	if _, err := acquireWithin(t.TempDir(), 150*time.Millisecond); !errors.Is(err, errQueued) {
		t.Errorf("another checkout with no arena record: err = %v, want errQueued", err)
	}
	if _, err := acquireWithin(root, 150*time.Millisecond); !errors.Is(err, errQueued) {
		t.Errorf("the same checkout again: err = %v, want errQueued — nothing re-enters an unnamed holder", err)
	}
}

// TestAcquireHostScope_SaysWhoItWaitsForOnlyWhenItWaits: the line a queued
// verify prints names the arena it is behind, and a verify that is not queued —
// the exclusion was free, or its runner's arena already holds it — prints
// nothing, so a runner's verify never claims to be waiting.
func TestAcquireHostScope_SaysWhoItWaitsForOnlyWhenItWaits(t *testing.T) {
	cleanTestHome(t)
	provisionHost(t, provisionedHost)
	root := arenaRoot(t, testArenaID)
	// The line is narration, which goes to stderr when someone is watching.
	cliObserved(t, true)

	var err error
	var unlock func()
	if out := captureStderr(t, func() { unlock, err = acquireWithin(root, 0) }); err != nil || out != "" {
		t.Fatalf("a free exclusion: err = %v, printed %q, want nothing", err, out)
	}
	defer unlock()

	var nested func()
	if out := captureStderr(t, func() { nested, err = acquireWithin(root, 5*time.Second) }); err != nil || out != "" {
		t.Fatalf("re-entering this arena's own exclusion: err = %v, printed %q, want nothing", err, out)
	}
	nested()

	out := captureStderr(t, func() { _, err = acquireWithin(arenaRoot(t, peerArenaID), 150*time.Millisecond) })
	if !errors.Is(err, errQueued) {
		t.Fatalf("a peer: err = %v, want errQueued", err)
	}
	if !strings.Contains(out, "Waiting") || !strings.Contains(out, testArenaID) {
		t.Errorf("a queued verify printed %q, want a waiting line naming the holder's arena %s", out, testArenaID)
	}
}

// TestAcquireHostScope_ABadArenaRecordIsRefused: a record that exists and does
// not name an arena is refused, never read around — reading around it would
// give the checkout a second identity, one its runner would not recognise.
func TestAcquireHostScope_ABadArenaRecordIsRefused(t *testing.T) {
	cleanTestHome(t)
	root := t.TempDir()
	writeFile(t, root, ".workspace/arena.json", `{"id":"not-an-arena"}`)

	unlock, err := acquireWithin(root, 100*time.Millisecond)
	if err == nil {
		unlock()
		t.Fatal("a malformed arena record was read around")
	}
	if !strings.Contains(err.Error(), "arena.json") {
		t.Errorf("the refusal must name the record, got: %v", err)
	}
	// Not the timeout: bin/verify maps errQueued to "retry later", and a
	// broken record is broken on every retry.
	if errors.Is(err, errQueued) {
		t.Errorf("err = %v; a malformed record is a refusal, not a wait that timed out", err)
	}
	if path, ok := hostscope.Path(); ok && Exists(path) {
		t.Errorf("a refused acquire still took the exclusion at %s", path)
	}
}

// TestAcquireHostScope_NoHostRecordQueuesAsAnUnnamedParty: an arena checkout
// on a machine with no host identity record cannot name its arena, so it takes
// the exclusion as a party nothing re-enters — granted when it is free, never
// refused for the missing record, and queued even behind a runner holding it
// for the same arena id. No runner holds it for such a machine: flow refuses to
// run without the host record, so the last case costs nothing real.
func TestAcquireHostScope_NoHostRecordQueuesAsAnUnnamedParty(t *testing.T) {
	cleanTestHome(t)
	root := arenaRoot(t, testArenaID)

	unlock, err := acquireWithin(root, 5*time.Second)
	if err != nil {
		t.Fatalf("a machine with no host record was refused a free exclusion: %v", err)
	}
	if got, ok := hostscope.Holder(); ok {
		t.Errorf("the exclusion names %v, want nobody — this machine has no host identity", got)
	}
	unlock()

	runner, _, err := hostscope.Acquire(context.Background(), hostscope.ArenaIdentity{Host: testHostID, Id: testArenaID})
	if err != nil {
		t.Fatal(err)
	}
	defer runner()
	if _, err := acquireWithin(root, 150*time.Millisecond); !errors.Is(err, errQueued) {
		t.Errorf("err = %v, want errQueued — nothing re-enters on an arena with no host", err)
	}
}

// TestAcquireHostScope_ABadHostRecordIsRefused: a host record that exists and
// does not name a machine is refused, never read around — reading around it
// would name the arena differently from its runner, which refuses it too.
func TestAcquireHostScope_ABadHostRecordIsRefused(t *testing.T) {
	cleanTestHome(t)
	provisionHost(t, `{"kind":"host","id":"livingroom-ubuntu"}`)
	root := arenaRoot(t, testArenaID)

	unlock, err := acquireWithin(root, 100*time.Millisecond)
	if err == nil {
		unlock()
		t.Fatal("a malformed host record was read around")
	}
	if !strings.Contains(err.Error(), "identity.json") {
		t.Errorf("the refusal must name the record, got: %v", err)
	}
	if errors.Is(err, errQueued) {
		t.Errorf("err = %v; a malformed record is a refusal, not a wait that timed out", err)
	}
	if path, ok := hostscope.Path(); ok && Exists(path) {
		t.Errorf("a refused acquire still took the exclusion at %s", path)
	}
}

// stepLog is a pipeline of steps that record the order they ran in, so the
// guarantees that matter — stop at the first failure, never push what was not
// blessed — can be asserted without taking a host lock or building a compiler
// (T2092). RunVerify's real pipeline is asserted separately, by name.
type stepLog struct {
	ran []string
}

func (l *stepLog) step(name string, err error) verifyStep {
	return verifyStep{"stage", name, func() error {
		l.ran = append(l.ran, name)
		return err
	}}
}

// verifyStepNames is the pipeline as a caller would read it: stage/step.
func verifyStepNames() []string {
	r := &verifyRun{root: "/nowhere"}
	var names []string
	for _, s := range r.steps() {
		names = append(names, s.stage+"/"+s.name)
	}
	return names
}

// TestVerifySteps_Order pins the sequence, which is where verify's guarantees
// live: the blessing is cleared before anything can change the tree, the build
// precedes the repairs (so `promise format` is the formatter `integration` then
// measures), and the measurement precedes the record.
func TestVerifySteps_Order(t *testing.T) {
	want := []string{
		"prepare/lock", "prepare/clear", "prepare/cache", "prepare/build",
		"repair/gofmt", "repair/promise format",
		"check/go vet", "check/structure",
		"measure/integration", "record/verified tree",
	}
	if got := verifyStepNames(); !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

// TestRunVerifySteps_StopsAtTheFirstFailureAndNamesIt is what makes the ordering
// above a guarantee rather than an intention: a step that failed ends the run,
// so nothing downstream has to re-check what happened upstream.
func TestRunVerifySteps_StopsAtTheFirstFailureAndNamesIt(t *testing.T) {
	var l stepLog
	got := runVerifySteps([]verifyStep{
		l.step("build", nil),
		l.step("go vet", errors.New("boom")),
		l.step("integration", nil),
	})
	if len(got) != 1 {
		t.Fatalf("stages = %+v, want the one stage the steps share", got)
	}
	var statuses []string
	for _, s := range got[0].Steps {
		statuses = append(statuses, s.Name+"="+s.Status)
	}
	if want := []string{"build=passed", "go vet=failed", "integration=not-run"}; !slices.Equal(statuses, want) {
		t.Errorf("steps = %v, want %v", statuses, want)
	}
	if d := got[0].Steps[1].Detail; d != "boom" {
		t.Errorf("the failed step's detail = %q, want what it said", d)
	}
	if want := []string{"build", "go vet"}; !slices.Equal(l.ran, want) {
		t.Errorf("ran %v, want %v — nothing may run after a failure", l.ran, want)
	}
	if res := (verifyResult{Stages: got}); failedStep(res.Stages) != "go vet" {
		t.Errorf("failedStep = %q, want go vet", failedStep(res.Stages))
	}
}

// TestRunVerifySteps_InterruptStopsBeforeTheNextStep covers Ctrl+C. The pipeline
// stops between steps rather than mid-step, and reports the step it had reached.
func TestRunVerifySteps_InterruptStopsBeforeTheNextStep(t *testing.T) {
	t.Cleanup(func() { interrupted.Store(0) })
	var l stepLog
	got := runVerifySteps([]verifyStep{
		l.step("build", nil),
		{"stage", "go vet", func() error {
			l.ran = append(l.ran, "go vet")
			interrupted.Store(1)
			return nil
		}},
		l.step("integration", nil),
	})
	if failedStep(got) != "go vet" || got[0].Steps[1].Detail != errInterrupted.Error() {
		t.Errorf("steps = %+v, want go vet failed as interrupted", got[0].Steps)
	}
	if want := []string{"build", "go vet"}; !slices.Equal(l.ran, want) {
		t.Errorf("ran %v, want %v", l.ran, want)
	}
}

// TestStepRoll_EndsWithTheVerdictLine: the human summary always ends with the
// one line saying whether the tree may be committed, after the failed step's
// own words, so a tail read carries all the evidence.
func TestStepRoll_EndsWithTheVerdictLine(t *testing.T) {
	stages := []stageResult{{Name: "check", Steps: []stepResult{
		{Name: "go vet", Status: stepFailed, ElapsedSeconds: "1.500", Detail: "x.go:3: unreachable code"},
		{Name: "structure", Status: stepNotRun, ElapsedSeconds: "0"},
	}}}
	failed := stepRoll(verifyResult{OK: false, Stages: stages})
	for _, want := range []string{"FAIL     go vet", "not-run  structure", "go vet failed:\n  x.go:3: unreachable code"} {
		if !strings.Contains(failed, want) {
			t.Errorf("roll lacks %q:\n%s", want, failed)
		}
	}
	if !strings.HasSuffix(failed, "❌ Verify FAILED: not safe to commit\n") {
		t.Errorf("a failed roll must end with the FAILED line:\n%s", failed)
	}
	if ok := stepRoll(verifyResult{OK: true, Tree: "abc"}); !strings.HasSuffix(ok, "✅ OK to Commit\n") || !strings.Contains(ok, "blessed tree abc") {
		t.Errorf("a passing roll must name the tree and end with OK:\n%s", ok)
	}
}

// TestAcquireHostScope_NoCacheDirectoryRefuses covers a host with no user
// cache directory: there is nowhere to hold the host-scope exclusion, and
// acquireVerifyLock refuses rather than running unserialized. A silent free pass
// would let two verifies interleave over the same machine with nothing to say
// so, and a runner's measurement beside them would report the load as much as
// the code (flow docs/gates-and-commands.md § Two scopes).
func TestAcquireHostScope_NoCacheDirectoryRefuses(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // os.UserHomeDir on Windows
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("LocalAppData", "") // the Windows user cache directory
	if path, ok := hostscope.Path(); ok {
		t.Skipf("this host still resolves the exclusion (%s) with every variable blanked", path)
	}

	unlock, err := acquireWithin(t.TempDir(), 100*time.Millisecond)
	if err == nil {
		unlock()
		t.Fatal("a host with nowhere to hold the exclusion ran unserialized")
	}
	if errors.Is(err, errQueued) {
		t.Errorf("err = %v; a missing directory is a refusal, not a wait that timed out", err)
	}
}

// TestHostScope_CreatesNothingUnderTheSharedHome (#102): taking the host-scope
// exclusion — by bin/verify or by bin/clean — creates nothing under ~/.promise,
// which belongs to the installed CLI. The exclusion is the orchestrator's and
// lives where flow keeps it (hostscope.Path). A fresh, redirected home makes
// "nothing" exact: ~/.promise must not even exist afterwards.
func TestHostScope_CreatesNothingUnderTheSharedHome(t *testing.T) {
	home := cleanTestHome(t)
	shared := filepath.Join(home, ".promise")

	lockPath, ok := hostscope.Path()
	if !ok {
		t.Fatal("hostscope.Path: no directory for the exclusion under the redirected home")
	}
	if rel, err := filepath.Rel(home, lockPath); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("hostscope.Path = %q, outside the redirected home %q — the test would take the host's exclusion", lockPath, home)
	}
	if rel, err := filepath.Rel(shared, lockPath); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("hostscope.Path = %q, which is under the shared home %q", lockPath, shared)
	}

	unlock, err := acquireWithin(t.TempDir(), time.Second)
	if err != nil {
		t.Fatalf("acquireVerifyLock: %v", err)
	}
	if !Exists(lockPath) {
		t.Errorf("the exclusion was not taken at %s", lockPath)
	}
	unlock()

	// bin/clean takes the same exclusion around its removal.
	if err := Clean(t.TempDir(), CleanOptions{Quiet: true}); err != nil {
		t.Fatalf("Clean: %v", err)
	}

	if Exists(shared) {
		t.Errorf("taking the verify lock created %s; nothing run from a worktree may write the shared home", shared)
	}
}
