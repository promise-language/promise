package common

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/promise-language/promise/tools/build/internal/hostscope"
)

// TestRunVerify_UnknownFlagReturnsUsageError verifies that passing an unknown
// flag causes RunVerify to return the usage error immediately (before any lock
// or filesystem side effects). Also confirms the usage string includes [--push].
// This is the wiring pin: parsing happens ahead of every side effect, so a
// mistyped flag cannot take the lock, clean, build or push.
func TestRunVerify_UnknownFlagReturnsUsageError(t *testing.T) {
	err := RunVerify(t.TempDir(), []string{"--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
	const want = verifyUsage
	if err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
}

// TestParseVerifyArgs_EveryFlagSetsItsOption checks that every documented flag
// parses into the option it names, and into no other.
//
// It asserts on parseVerifyArgs rather than running RunVerify, which is what
// this coverage used to do (T2084). Running the pipeline to prove a flag parsed
// was both destructive and weak: `--shared --clean` resolved CleanHome to the
// host's ~/.promise and deleted it — the installed toolchain included — and the
// only assertion (the error is not a "usage:" error) was equally satisfied by a
// flag parsed into the wrong field, or by an ErrLockTimeout returned before the
// pipeline it claimed to exercise ever started.
func TestParseVerifyArgs_EveryFlagSetsItsOption(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want verifyOptions
	}{
		{"clean", []string{"--clean"}, verifyOptions{clean: true}},
		{"push", []string{"--push"}, verifyOptions{push: true}},
		{"lock-timeout", []string{"--lock-timeout=100ms"}, verifyOptions{lockTimeout: 100 * time.Millisecond}},
		{"lock-timeout separate value", []string{"--lock-timeout", "10m"}, verifyOptions{lockTimeout: 10 * time.Minute}},
		{"single dash", []string{"-clean"}, verifyOptions{clean: true}},
		{"none", nil, verifyOptions{}},
		// 0 is not "do not wait" — it is the zero value the absent flag leaves
		// behind, which acquireVerifyLock reads as "wait indefinitely". Anyone
		// reaching for --lock-timeout=0 to make verify give up immediately gets
		// the opposite, so the collision is pinned rather than left to be
		// rediscovered.
		{"zero timeout is the unbounded sentinel", []string{"--lock-timeout=0s"}, verifyOptions{}},
		{"repeated flag", []string{"--clean", "--clean"}, verifyOptions{clean: true}},
		{"last timeout wins", []string{"--lock-timeout=1s", "--lock-timeout=2s"}, verifyOptions{lockTimeout: 2 * time.Second}},
		{
			"all together",
			[]string{"--clean", "--push", "--lock-timeout=1s"},
			verifyOptions{clean: true, push: true, lockTimeout: time.Second},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVerifyArgs(tc.args)
			if err != nil {
				t.Fatalf("parseVerifyArgs(%v) = %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseVerifyArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// TestParseVerifyArgs_VariantOptionsAreRefused is the flag half of "one
// measurement, one meaning of blessed" (T2170).
//
// --wasm, --wasm-web, --shared and --local each used to change what a run
// measured or where it measured it, while the record it wrote said none of it:
// `bin/verify` and `bin/verify --shared --wasm` blessed the same tree id for two
// different measurements. They are REFUSED rather than ignored — a silent no-op
// would leave every caller that still passes one believing it got the suite it
// asked for, and the WASM suites are real measurements, addressed by name
// (`bin/gate wasm-test`, `bin/gate wasm-web-test`).
func TestParseVerifyArgs_VariantOptionsAreRefused(t *testing.T) {
	for _, tc := range []struct {
		args []string
		// names is what the refusal must point the caller at: losing a flag and
		// losing a suite are different facts, and the two WASM suites are still
		// measured, by name, on a schedule.
		names string
	}{
		{[]string{"--wasm"}, "bin/gate wasm-test"},
		{[]string{"--wasm-web"}, "bin/gate wasm-web-test"},
		{[]string{"--shared"}, "no command run from a worktree addresses ~/.promise"},
		{[]string{"--local"}, ".promise-home/"},
		{[]string{"--clean", "--wasm"}, "bin/gate wasm-test"},
		{[]string{"--shared", "--clean"}, "no command run from a worktree addresses ~/.promise"},
		{[]string{"-wasm"}, "bin/gate wasm-test"},
	} {
		got, err := parseVerifyArgs(tc.args)
		if err == nil {
			t.Errorf("parseVerifyArgs(%v) = %+v, want a refusal", tc.args, got)
			continue
		}
		if !strings.Contains(err.Error(), tc.names) {
			t.Errorf("parseVerifyArgs(%v) = %q, want it to name %q", tc.args, err, tc.names)
		}
		if !strings.Contains(err.Error(), verifyUsage) {
			t.Errorf("parseVerifyArgs(%v) = %q, want it to carry the usage line", tc.args, err)
		}
		if got != (verifyOptions{}) {
			t.Errorf("a rejected command line must yield zero options, got %+v", got)
		}
	}
}

// TestParseVerifyArgs_ATypoIsNotARetiredFlag: the two refusals are different on
// purpose. A retired option names where its measurement went; a mistyped one has
// nowhere to point, and inventing a destination for it would be a lie.
func TestParseVerifyArgs_ATypoIsNotARetiredFlag(t *testing.T) {
	_, err := parseVerifyArgs([]string{"--wsam"})
	if err == nil || err.Error() != verifyUsage {
		t.Errorf("parseVerifyArgs(--wsam) = %v, want exactly the usage line", err)
	}
}

// TestParseVerifyArgs_Rejections covers the error paths: an unknown flag gets
// the usage string, and a bad --lock-timeout value gets a flag-specific message
// naming the offending flag rather than the generic usage line.
func TestParseVerifyArgs_Rejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown flag", []string{"--unknown"}, "usage: bin/verify"},
		{"missing value", []string{"--lock-timeout"}, "-lock-timeout requires a duration value"},
		{"unparseable value", []string{"--lock-timeout=nope"}, "-lock-timeout: invalid duration"},
		{"negative value", []string{"--lock-timeout=-5s"}, "-lock-timeout: invalid duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVerifyArgs(tc.args)
			if err == nil {
				t.Fatalf("parseVerifyArgs(%v) = %+v, want an error", tc.args, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should contain %q", err.Error(), tc.want)
			}
			if got != (verifyOptions{}) {
				t.Errorf("a rejected command line must yield zero options, got %+v", got)
			}
		})
	}
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
)

// TestAcquireVerifyLock_NamesThisCheckoutsArena: the record a refused party
// reads names the arena this checkout is, so a runner of the same arena can
// recognise it — and nothing else, since the arena is what the exclusion is
// re-entered on.
func TestAcquireVerifyLock_NamesThisCheckoutsArena(t *testing.T) {
	cleanTestHome(t)
	root := arenaRoot(t, testArenaID)

	unlock, err := acquireVerifyLock(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	got, ok := hostscope.Holder()
	want, aerr := hostscope.ArenaAt(root)
	if aerr != nil {
		t.Fatal(aerr)
	}
	if !ok || got != want {
		t.Errorf("the exclusion names %v (%v), want this checkout's arena %v", got, ok, want)
	}
}

// TestAcquireVerifyLock_TimesOutWhenAPeerHoldsIt: another checkout holds the
// exclusion, and a bounded acquire returns ErrLockTimeout (not a verification
// failure) once its bound elapses.
func TestAcquireVerifyLock_TimesOutWhenAPeerHoldsIt(t *testing.T) {
	cleanTestHome(t)
	unlock, err := acquireVerifyLock(arenaRoot(t, peerArenaID), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	start := time.Now()
	_, err = acquireVerifyLock(arenaRoot(t, testArenaID), 150*time.Millisecond)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("returned after %v, want it to wait ~the lock timeout before giving up", elapsed)
	}
}

// TestAcquireVerifyLock_UnboundedAcquiresAfterRelease confirms lockTimeout=0
// waits (does not time out) and succeeds once the holder releases. The waiter
// has no arena record — a contributor clone — and still queues like any party.
func TestAcquireVerifyLock_UnboundedAcquiresAfterRelease(t *testing.T) {
	cleanTestHome(t)
	unlock, err := acquireVerifyLock(arenaRoot(t, peerArenaID), 0)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan error, 1)
	go func() {
		inner, ierr := acquireVerifyLock(t.TempDir(), 0)
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

// TestAcquireVerifyLock_LetsTheHoldingArenaStraightThrough is the runner case
// (#96): a flow runner holds the exclusion for this checkout's arena and spawns
// bin/verify, which must be let straight through rather than queue behind its
// own parent inside the run's allowance. Its release must not hand the machine
// to a peer while the runner still holds it.
func TestAcquireVerifyLock_LetsTheHoldingArenaStraightThrough(t *testing.T) {
	cleanTestHome(t)
	root := arenaRoot(t, testArenaID)
	holder, err := hostscope.ArenaAt(root)
	if err != nil {
		t.Fatal(err)
	}
	// The runner's acquisition: flow takes it as hostscope.Acquire(ctx,
	// flow.ArenaAt(worktree)), which names the same pair.
	runner, _, err := hostscope.Acquire(context.Background(), holder)
	if err != nil {
		t.Fatal(err)
	}
	defer runner()

	nested, err := acquireVerifyLock(root, 5*time.Second)
	if err != nil {
		t.Fatalf("a verify in the holding arena could not enter the exclusion its arena holds: %v", err)
	}
	nested()

	if _, err := acquireVerifyLock(arenaRoot(t, peerArenaID), 150*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("a peer was not excluded after the nested release (err = %v); the runner still holds it", err)
	}
}

// TestAcquireVerifyLock_AHandRunVerifyHoldsOffARunner is the other direction of
// #96: a verify run by hand in one checkout holds the exclusion, and a runner of
// another arena — taking it exactly as flow does, before it spawns its own verify
// — queues behind it and is granted when it ends, with the queue reported as a
// wait (what the SDK files as CommandRun.Waited, outside the allowance). The
// defect this replaced was a hand-run verify holding a lock of its own that the
// runner's acquisition never saw.
func TestAcquireVerifyLock_AHandRunVerifyHoldsOffARunner(t *testing.T) {
	cleanTestHome(t)
	hand, err := acquireVerifyLock(arenaRoot(t, peerArenaID), 0)
	if err != nil {
		t.Fatal(err)
	}
	runnerArena, err := hostscope.ArenaAt(arenaRoot(t, testArenaID))
	if err != nil {
		hand()
		t.Fatal(err)
	}

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

// TestAcquireVerifyLock_CheckoutsWithNoArenaRecordExcludeEachOther: a checkout no
// flow provisioned (a contributor clone, a CI runner) has no arena to re-enter
// on, so it is a party like any other — two of them exclude each other, and so
// does a second acquisition from the same one, which is why `verify --clean`
// calls cleanLocked rather than Clean.
func TestAcquireVerifyLock_CheckoutsWithNoArenaRecordExcludeEachOther(t *testing.T) {
	cleanTestHome(t)
	root := t.TempDir()
	unlock, err := acquireVerifyLock(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	if _, err := acquireVerifyLock(t.TempDir(), 150*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		t.Errorf("another checkout with no arena record: err = %v, want ErrLockTimeout", err)
	}
	if _, err := acquireVerifyLock(root, 150*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		t.Errorf("the same checkout again: err = %v, want ErrLockTimeout — nothing re-enters an unnamed holder", err)
	}
}

// TestAcquireVerifyLock_SaysWhoItWaitsForOnlyWhenItWaits: the line a queued
// verify prints names the arena it is behind, and a verify that is not queued —
// the exclusion was free, or its runner's arena already holds it — prints
// nothing, so a runner's verify never claims to be waiting.
func TestAcquireVerifyLock_SaysWhoItWaitsForOnlyWhenItWaits(t *testing.T) {
	cleanTestHome(t)
	root := arenaRoot(t, testArenaID)

	var err error
	var unlock func()
	if out := captureStdout(t, func() { unlock, err = acquireVerifyLock(root, 0) }); err != nil || out != "" {
		t.Fatalf("a free exclusion: err = %v, printed %q, want nothing", err, out)
	}
	defer unlock()

	var nested func()
	if out := captureStdout(t, func() { nested, err = acquireVerifyLock(root, 5*time.Second) }); err != nil || out != "" {
		t.Fatalf("re-entering this arena's own exclusion: err = %v, printed %q, want nothing", err, out)
	}
	nested()

	out := captureStdout(t, func() { _, err = acquireVerifyLock(arenaRoot(t, peerArenaID), 150*time.Millisecond) })
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("a peer: err = %v, want ErrLockTimeout", err)
	}
	if !strings.Contains(out, "Waiting") || !strings.Contains(out, testArenaID) {
		t.Errorf("a queued verify printed %q, want a waiting line naming the holder's arena %s", out, testArenaID)
	}
}

// TestAcquireVerifyLock_ABadArenaRecordIsRefused: a record that exists and does
// not name an arena is refused, never read around — reading around it would
// give the checkout a second identity, one its runner would not recognise.
func TestAcquireVerifyLock_ABadArenaRecordIsRefused(t *testing.T) {
	cleanTestHome(t)
	root := t.TempDir()
	writeFile(t, root, ".workspace/arena.json", `{"id":"not-an-arena"}`)

	unlock, err := acquireVerifyLock(root, 100*time.Millisecond)
	if err == nil {
		unlock()
		t.Fatal("a malformed arena record was read around")
	}
	if !strings.Contains(err.Error(), "arena.json") {
		t.Errorf("the refusal must name the record, got: %v", err)
	}
	// Not the timeout: bin/verify maps ErrLockTimeout to "retry later", and a
	// broken record is broken on every retry.
	if errors.Is(err, ErrLockTimeout) {
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
	return verifyStep{name, func() error {
		l.ran = append(l.ran, name)
		return err
	}}
}

// verifyStepNames is the pipeline as a caller would read it.
func verifyStepNames(opts verifyOptions) []string {
	r := &verifyRun{root: "/nowhere", opts: opts}
	var names []string
	for _, s := range r.steps() {
		names = append(names, s.name)
	}
	return names
}

// TestVerifySteps_Order pins the sequence, which is where verify's guarantees
// live: the blessing is cleared before anything can change the tree, the build
// precedes the repairs (so `promise format` is the formatter `integration` then
// measures), the measurement precedes the record, and the record precedes the
// push.
func TestVerifySteps_Order(t *testing.T) {
	want := []string{
		"lock", "clear", "cache", "build",
		"format go", "format promise",
		"check go", "check structure",
		"integration", "record",
	}
	if got := verifyStepNames(verifyOptions{}); !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

// TestVerifySteps_OptionalStepsSitWhereTheyBelong covers the two conditional
// steps. --clean must run before the cache is set up (it recreates the home
// empty), and --push must be last of all, after the record.
func TestVerifySteps_OptionalStepsSitWhereTheyBelong(t *testing.T) {
	both := verifyStepNames(verifyOptions{clean: true, push: true})
	if i, j := slices.Index(both, "clean"), slices.Index(both, "cache"); i < 0 || i > j {
		t.Errorf("clean at %d, cache at %d — clean must precede the cache setup: %v", i, j, both)
	}
	if got, want := both[len(both)-1], "push"; got != want {
		t.Errorf("last step = %q, want %q: %v", got, want, both)
	}
	if got, want := both[len(both)-2], "record"; got != want {
		t.Errorf("step before push = %q, want %q: %v", got, want, both)
	}
	for _, name := range []string{"clean", "push"} {
		if slices.Contains(verifyStepNames(verifyOptions{}), name) {
			t.Errorf("%q must not run without its flag", name)
		}
	}
}

// TestRunVerifySteps_StopsAtTheFirstFailureAndNamesIt is what makes the ordering
// above a guarantee rather than an intention: a step that failed ends the run,
// so nothing downstream has to re-check what happened upstream.
func TestRunVerifySteps_StopsAtTheFirstFailureAndNamesIt(t *testing.T) {
	var l stepLog
	boom := errors.New("boom")
	failed, err := runVerifySteps([]verifyStep{
		l.step("build", nil),
		l.step("check go", boom),
		l.step("integration", nil),
	})
	if failed != "check go" {
		t.Errorf("failed step = %q, want %q", failed, "check go")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap boom", err)
	}
	if !strings.HasPrefix(err.Error(), "check go: ") {
		t.Errorf("err = %q, want it to name the step that failed", err)
	}
	if want := []string{"build", "check go"}; !slices.Equal(l.ran, want) {
		t.Errorf("ran %v, want %v — nothing may run after a failure", l.ran, want)
	}
}

// TestRunVerifySteps_PushNeedsARecordedBlessing pins the one step whose effect
// leaves the machine: --push is downstream of the record, so a blessing that
// could not be written stops the run before anything is published.
func TestRunVerifySteps_PushNeedsARecordedBlessing(t *testing.T) {
	var l stepLog
	_, err := runVerifySteps([]verifyStep{
		l.step("integration", nil),
		l.step("record", errors.New("not gitignored")),
		l.step("push", nil),
	})
	if err == nil {
		t.Fatal("a failed record must fail the run")
	}
	if slices.Contains(l.ran, "push") {
		t.Errorf("pushed after a record that failed: %v", l.ran)
	}
}

// TestRunVerifySteps_InterruptStopsBeforeTheNextStep covers Ctrl+C. The pipeline
// stops between steps rather than mid-step, and reports the step it had reached.
func TestRunVerifySteps_InterruptStopsBeforeTheNextStep(t *testing.T) {
	t.Cleanup(func() { interrupted.Store(0) })
	var l stepLog
	failed, err := runVerifySteps([]verifyStep{
		l.step("build", nil),
		{"check go", func() error {
			l.ran = append(l.ran, "check go")
			interrupted.Store(1)
			return nil
		}},
		l.step("integration", nil),
	})
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("err = %v, want errInterrupted", err)
	}
	if failed != "check go" {
		t.Errorf("failed step = %q, want the step the run had reached", failed)
	}
	if want := []string{"build", "check go"}; !slices.Equal(l.ran, want) {
		t.Errorf("ran %v, want %v", l.ran, want)
	}
}

// TestRunVerify_HonoursTheParsedLockTimeout is the accepted-command-line half
// of the wiring, as TestRunVerify_UnknownFlagReturnsUsageError is the rejected
// half: --lock-timeout has to survive the trip from parseVerifyArgs into
// acquireVerifyLock, and the resulting ErrLockTimeout has to come back
// unwrapped.
//
// Both halves are load-bearing for the tracker runner, which distinguishes
// "another verify held the lock, retry next turn" from "verification failed" by
// errors.Is on this value. A regression that dropped opts.lockTimeout would not
// fail anything visibly — it would wait forever, which is exactly how the bug
// this test guards against would present.
//
// The exclusion is this test's own: the user dirs are redirected, so
// acquireVerifyLock resolves to a private cache dir rather than the host's,
// whose exclusion an outer bin/verify holds while these tests run. The holder
// is a peer checkout taking it through acquireVerifyLock — the same function
// the run reaches — so this test keeps no second spelling of where it lives.
func TestRunVerify_HonoursTheParsedLockTimeout(t *testing.T) {
	cleanTestHome(t)

	unlock, err := acquireVerifyLock(arenaRoot(t, peerArenaID), 0)
	if err != nil {
		t.Fatalf("acquireVerifyLock: %v", err)
	}

	// A stale blessing to watch: clearBlessing is RunVerify's first step
	// after the lock, so this file surviving proves the run gave up at the lock
	// and never entered the pipeline.
	root := t.TempDir()
	writeFile(t, root, ".workspace/verified-tree", "stale-blessing\n")

	done := make(chan error, 1)
	go func() { done <- RunVerify(root, []string{"--lock-timeout=100ms"}) }()

	select {
	case err := <-done:
		unlock()
		if !errors.Is(err, ErrLockTimeout) {
			t.Fatalf("RunVerify = %v, want ErrLockTimeout", err)
		}
	case <-time.After(30 * time.Second):
		// Deliberately not unlocking: the run is still blocked on the lock, and
		// releasing it now would send a verify with a broken timeout into the
		// pipeline — format, build, test and push — on a temp root, after
		// t.Setenv has already restored the real HOME.
		t.Fatal("RunVerify ignored --lock-timeout=100ms and is still waiting for the lock")
	}

	if !Exists(filepath.Join(root, ".workspace", "verified-tree")) {
		t.Error("a run that timed out on the lock must not have started the pipeline")
	}
}

// runVerifyStringArgs returns every string literal appearing inside each call to
// RunVerify in the given file, keyed by call site. Collecting literals from the
// whole call expression rather than matching an argument shape keeps it working
// for both RunVerify(dir, []string{...}) and any future spelling.
func runVerifyStringArgs(fset *token.FileSet, file *ast.File) map[string][]string {
	sites := map[string][]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "RunVerify" {
			return true
		}
		site := fset.Position(call.Pos()).String()
		var args []string
		ast.Inspect(call, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					args = append(args, s)
				}
			}
			return true
		})
		sites[site] = NormalizeArgs(args)
		return true
	})
	return sites
}

// TestRunVerifyStringArgs exercises the scanner against synthetic source, so
// TestNoTestDrivesRunVerifyWithPush means something. Run only over the real
// (clean) tree, the scanner could match nothing at all — a typo'd function name,
// or a walk that never descends into the argument slice — and stay green
// forever.
func TestRunVerifyStringArgs(t *testing.T) {
	const src = `package p
func f() {
	RunVerify(dir, []string{"--clean", "--lock-timeout=30s"})
	RunVerify(dir, []string{"--push"})
	RunVerify(dir, nil)
	RunClean(dir, []string{"--push"})
	notRunVerify(dir, []string{"--push"})
}`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic_test.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	sites := runVerifyStringArgs(fset, file)
	if len(sites) != 3 {
		t.Fatalf("found %d RunVerify call sites, want 3: %v", len(sites), sites)
	}

	var withPush, total int
	for _, args := range sites {
		total++
		if slices.Contains(args, "-push") {
			withPush++
		}
	}
	if withPush != 1 {
		t.Errorf("%d of %d call sites pass --push, want exactly 1 — the scanner is not reading the argument slice", withPush, total)
	}
	// --lock-timeout=30s must arrive normalized, the same way the parser sees it.
	var sawTimeout bool
	for _, args := range sites {
		if slices.Contains(args, "-lock-timeout") && slices.Contains(args, "30s") {
			sawTimeout = true
		}
	}
	if !sawTimeout {
		t.Error("literals must be normalized like a real command line (--lock-timeout=30s → -lock-timeout 30s)")
	}
}

// TestNoTestDrivesRunVerifyWithPush forbids this package's tests from handing
// RunVerify the one flag whose effect leaves the machine.
//
// The TestMain guard watches ~/.promise and the Go test cache, so a test that
// reached the clean is caught after the fact. A push cannot be caught that way:
// it publishes to the real remote, from whatever checkout the test pointed at,
// and there is nothing to compare afterwards. Before T2084 a flag-acceptance
// test did pass --push, and was harmless only because FormatGo happened to fail
// first on an empty temp root — one reordering away from a real push.
//
// Flag acceptance is a parser question: assert on parseVerifyArgs.
func TestNoTestDrivesRunVerifyWithPush(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var scanned int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		scanned++
		for site, args := range runVerifyStringArgs(fset, file) {
			if slices.Contains(args, "-push") {
				t.Errorf("%s: RunVerify must never be driven with --push from a test; "+
					"assert on parseVerifyArgs instead", site)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no _test.go files — the guard is looking in the wrong place")
	}
}

// TestAcquireVerifyLock_NoCacheDirectoryRefuses covers a host with no user
// cache directory: there is nowhere to hold the host-scope exclusion, and
// acquireVerifyLock refuses rather than running unserialized. A silent free pass
// would let two verifies interleave over the same machine with nothing to say
// so, and a runner's measurement beside them would report the load as much as
// the code (flow docs/gates-and-commands.md § Two scopes).
func TestAcquireVerifyLock_NoCacheDirectoryRefuses(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // os.UserHomeDir on Windows
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("LocalAppData", "") // the Windows user cache directory
	if path, ok := hostscope.Path(); ok {
		t.Skipf("this host still resolves the exclusion (%s) with every variable blanked", path)
	}

	unlock, err := acquireVerifyLock(t.TempDir(), 100*time.Millisecond)
	if err == nil {
		unlock()
		t.Fatal("a host with nowhere to hold the exclusion ran unserialized")
	}
	if errors.Is(err, ErrLockTimeout) {
		t.Errorf("err = %v; a missing directory is a refusal, not a wait that timed out", err)
	}
}

// TestVerifyLock_CreatesNothingUnderTheSharedHome (#102): taking the host-scope
// exclusion — by bin/verify or by bin/clean — creates nothing under ~/.promise,
// which belongs to the installed CLI. The exclusion is the orchestrator's and
// lives where flow keeps it (hostscope.Path). A fresh, redirected home makes
// "nothing" exact: ~/.promise must not even exist afterwards.
func TestVerifyLock_CreatesNothingUnderTheSharedHome(t *testing.T) {
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

	unlock, err := acquireVerifyLock(t.TempDir(), time.Second)
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
