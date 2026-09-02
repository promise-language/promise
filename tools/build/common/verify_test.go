package common

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRunVerify_UnknownFlagReturnsUsageError verifies that passing an unknown
// flag causes RunVerify to return the usage error immediately (before any lock
// or filesystem side effects). Also confirms the usage string includes [--push].
func TestRunVerify_UnknownFlagReturnsUsageError(t *testing.T) {
	err := RunVerify(t.TempDir(), []string{"--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
	const want = "usage: bin/verify [--shared] [--wasm] [--wasm-web] [--clean] [--push] [--lock-timeout=<dur>]"
	if err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
}

// TestRunVerify_PushFlagIsValid verifies that --push is accepted by the arg
// validation switch and does not produce a usage error.
// --shared is included to skip SetupLocalCache env-var side effects.
// --lock-timeout=100ms prevents blocking on the global ~/.promise/verify.lock
// when the test runs inside an outer bin/verify (which holds the lock).
// The pipeline will fail in a temp dir (no source code), but the error must
// not be the usage-validation error.
func TestRunVerify_PushFlagIsValid(t *testing.T) {
	err := RunVerify(t.TempDir(), []string{"--shared", "--push", "--lock-timeout=100ms"})
	if err != nil && strings.HasPrefix(err.Error(), "usage:") {
		t.Errorf("--push treated as unknown flag, got usage error: %v", err)
	}
}

// TestRunVerify_AllKnownFlagsAreValid checks that every documented flag
// individually passes arg validation.
// --lock-timeout=100ms prevents blocking on the global ~/.promise/verify.lock
// when the test runs inside an outer bin/verify (which holds the lock).
func TestRunVerify_AllKnownFlagsAreValid(t *testing.T) {
	for _, flag := range []string{"--local", "--shared", "--wasm", "--wasm-web", "--clean", "--push"} {
		// Always pair with --shared to avoid SetupLocalCache env-var side effects.
		// Always add --lock-timeout=100ms to avoid blocking when the global lock
		// is held by an outer bin/verify run (e.g. the one running these tests).
		args := []string{"--shared", "--lock-timeout=100ms", flag}
		if flag == "--shared" {
			args = []string{"--shared", "--lock-timeout=100ms"}
		}
		err := RunVerify(t.TempDir(), args)
		if err != nil && strings.HasPrefix(err.Error(), "usage:") {
			t.Errorf("flag %q treated as unknown: %v", flag, err)
		}
	}
}

func TestAcquireVerifyLock_WritesRepoDir(t *testing.T) {
	// Override lock dir to a temp directory so we don't conflict with real runs.
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "verify.lock")

	unlock, err := acquireVerifyLockIn(lockPath, "/home/user/my-repo", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	// Holder metadata is recorded in the sibling .owner file (see
	// acquireVerifyLockIn — lockPath itself carries a mandatory byte-0 lock on
	// Windows and cannot be read while held).
	data, err := os.ReadFile(lockPath + ".owner")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(data))
	if got != "/home/user/my-repo" {
		t.Errorf("owner file = %q, want %q", got, "/home/user/my-repo")
	}
}

// TestRunVerify_LockTimeoutFlagIsValid confirms --lock-timeout with a duration
// value passes arg validation (NormalizeArgs splits --lock-timeout=100ms into two
// tokens). The pipeline fails later in a temp dir, but never with a usage error.
// Using 100ms (not a large value) so the test doesn't block when the global lock
// is held by an outer bin/verify run.
func TestRunVerify_LockTimeoutFlagIsValid(t *testing.T) {
	err := RunVerify(t.TempDir(), []string{"--shared", "--lock-timeout=100ms"})
	if err != nil && strings.HasPrefix(err.Error(), "usage:") {
		t.Errorf("--lock-timeout treated as unknown flag, got usage error: %v", err)
	}
}

// TestRunVerify_LockTimeoutInvalidDuration rejects a non-duration value with a
// clear, flag-specific error (not the generic usage error).
func TestRunVerify_LockTimeoutInvalidDuration(t *testing.T) {
	err := RunVerify(t.TempDir(), []string{"--lock-timeout=nope"})
	if err == nil {
		t.Fatal("expected error for invalid --lock-timeout duration, got nil")
	}
	if !strings.Contains(err.Error(), "lock-timeout") {
		t.Errorf("error %q should name the offending flag", err.Error())
	}
}

// TestAcquireVerifyLock_TimesOutWhenHeld holds the lock, then a second bounded
// acquire on the same path returns ErrLockTimeout (not a verification failure).
func TestAcquireVerifyLock_TimesOutWhenHeld(t *testing.T) {
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "verify.lock")

	unlock, err := acquireVerifyLockIn(lockPath, "/holder", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	start := time.Now()
	_, err = acquireVerifyLockIn(lockPath, "/waiter", 150*time.Millisecond)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("returned after %v, want it to wait ~the lock timeout before giving up", elapsed)
	}
}

// TestAcquireVerifyLock_UnboundedAcquiresAfterRelease confirms lockTimeout=0
// waits (does not time out) and succeeds once the holder releases.
func TestAcquireVerifyLock_UnboundedAcquiresAfterRelease(t *testing.T) {
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "verify.lock")

	unlock, err := acquireVerifyLockIn(lockPath, "/holder", 0)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan error, 1)
	go func() {
		inner, ierr := acquireVerifyLockIn(lockPath, "/waiter", 0)
		if ierr == nil {
			inner()
		}
		acquired <- ierr
	}()

	// Give the waiter a moment to start blocking, then release.
	time.Sleep(100 * time.Millisecond)
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

func TestAcquireVerifyLock_ClearsOnUnlock(t *testing.T) {
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "verify.lock")

	unlock, err := acquireVerifyLockIn(lockPath, "/home/user/my-repo", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Unlock should clear the holder metadata.
	unlock()

	if _, err := os.Stat(lockPath + ".owner"); !os.IsNotExist(err) {
		t.Errorf("owner file should be removed after unlock, stat err = %v", err)
	}
}

// verifyPhaseStubs builds a verifySuites whose suites all pass, plus counters
// recording how many times each was invoked.
type verifyPhaseCounts struct{ goN, toolsN, flowsN, promiseN int }

func stubSuites(c *verifyPhaseCounts, goErr, toolsErr, flowsErr error) verifySuites {
	return verifySuites{
		goTests:    func(string) error { c.goN++; return goErr },
		toolsTests: func(string) error { c.toolsN++; return toolsErr },
		flowsTests: func(string) (bool, error) { c.flowsN++; return false, flowsErr },
		promiseTests: func(_, _ string) (string, error) {
			c.promiseN++
			return "1 passed, 0 failed (1 files, 0.001s)", nil
		},
	}
}

// TestRunVerifyTestPhases_GoFailureSkipsPromise is the T1888 abort: when the
// compiler's Go tests fail there is nothing to learn from spending the
// remaining minutes on ~20k Promise tests, so they must not run.
func TestRunVerifyTestPhases_GoFailureSkipsPromise(t *testing.T) {
	var c verifyPhaseCounts
	res, err := runVerifyTestPhases(t.TempDir(), true /*wasm*/, true, /*wasmWeb*/
		stubSuites(&c, errors.New("boom"), nil, nil))
	if err != nil {
		t.Fatalf("unexpected abort error: %v", err)
	}
	if c.promiseN != 0 {
		t.Errorf("promise tests ran %d times after a Go failure; want 0", c.promiseN)
	}
	if !res.promiseSkipped {
		t.Error("promiseSkipped should be set so the summary can say why")
	}
	if len(res.failures) != 1 || res.failures[0] != "go tests" {
		t.Errorf("failures = %v, want [go tests]", res.failures)
	}
	// The cheap suites still all run, so their failures are visible together.
	if c.goN != 1 || c.toolsN != 1 || c.flowsN != 1 {
		t.Errorf("go/tools/flows ran %d/%d/%d times; want 1/1/1", c.goN, c.toolsN, c.flowsN)
	}
}

// TestRunVerifyTestPhases_ToolsOrFlowsFailureSkipsPromise confirms the abort is
// keyed on any of the three Go suites, not just the compiler's.
func TestRunVerifyTestPhases_ToolsOrFlowsFailureSkipsPromise(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tools, flows error
		wantFailure  string
	}{
		{"tools", errors.New("boom"), nil, "tools go tests"},
		{"flows", nil, errors.New("boom"), "flows go tests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c verifyPhaseCounts
			res, err := runVerifyTestPhases(t.TempDir(), false, false,
				stubSuites(&c, nil, tc.tools, tc.flows))
			if err != nil {
				t.Fatalf("unexpected abort error: %v", err)
			}
			if c.promiseN != 0 {
				t.Errorf("promise tests ran %d times; want 0", c.promiseN)
			}
			if !res.promiseSkipped {
				t.Error("promiseSkipped should be set")
			}
			if len(res.failures) != 1 || res.failures[0] != tc.wantFailure {
				t.Errorf("failures = %v, want [%s]", res.failures, tc.wantFailure)
			}
		})
	}
}

// TestRunVerifyTestPhases_CleanGoRunReachesPromise is the other half: with the
// Go suites green the Promise phases run exactly as before, once per target.
func TestRunVerifyTestPhases_CleanGoRunReachesPromise(t *testing.T) {
	if Which("wasmtime") == "" || Which("node") == "" {
		t.Skip("wasmtime/node not installed — the wasm phases would abort early")
	}
	var c verifyPhaseCounts
	res, err := runVerifyTestPhases(t.TempDir(), true, true, stubSuites(&c, nil, nil, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.promiseSkipped {
		t.Error("promiseSkipped must be false when the Go suites pass")
	}
	if len(res.failures) != 0 {
		t.Errorf("failures = %v, want none", res.failures)
	}
	if c.promiseN != 3 {
		t.Errorf("promise tests ran %d times; want 3 (host, wasm32-wasi, wasm32-web)", c.promiseN)
	}
	if res.hostOutput == "" || res.wasmOutput == "" || res.wasmWebOutput == "" {
		t.Errorf("per-target output was not captured: %+v", res)
	}
}

// TestRunVerifyTestPhases_HostOnlyRunsOnePromisePhase covers the default
// (no --wasm/--wasm-web) path.
func TestRunVerifyTestPhases_HostOnlyRunsOnePromisePhase(t *testing.T) {
	var c verifyPhaseCounts
	res, err := runVerifyTestPhases(t.TempDir(), false, false, stubSuites(&c, nil, nil, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.promiseN != 1 {
		t.Errorf("promise tests ran %d times; want 1 (host only)", c.promiseN)
	}
	if res.wasmOutput != "" || res.wasmWebOutput != "" {
		t.Errorf("wasm output should be empty when not requested: %+v", res)
	}
}

// fakeToolPath puts executable stubs named after each tool on PATH, so the
// wasm phases' Which() probes resolve without wasmtime or Node.js installed.
// The stubs are never executed — runVerifyTestPhases only asks whether they
// exist before calling the (injected) promise-test suite.
func fakeToolPath(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		p := filepath.Join(dir, n+ExeSuffix())
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// TestRunVerifyTestPhases_AllTargetsRunWhenGoIsGreen is the deterministic twin
// of the wasmtime/node-gated test above: with the tool probes satisfied by
// stubs, all three Promise phases must run and their per-target output be kept
// separately, on every machine.
func TestRunVerifyTestPhases_AllTargetsRunWhenGoIsGreen(t *testing.T) {
	fakeToolPath(t, "wasmtime", "node")
	var c verifyPhaseCounts
	var targets []string
	s := stubSuites(&c, nil, nil, nil)
	s.promiseTests = func(_, target string) (string, error) {
		targets = append(targets, target)
		return "out:" + target, nil
	}
	res, err := runVerifyTestPhases(t.TempDir(), true, true, s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := strings.Join(targets, ","), ",wasm32-wasi,wasm32-web"; got != want {
		t.Errorf("promise phases ran for %q, want %q (host first)", got, want)
	}
	if res.hostOutput != "out:" || res.wasmOutput != "out:wasm32-wasi" || res.wasmWebOutput != "out:wasm32-web" {
		t.Errorf("per-target output was mixed up: %+v", res)
	}
	if res.promiseSkipped || len(res.failures) != 0 {
		t.Errorf("a green run must record no failures: %+v", res)
	}
}

// TestRunVerifyTestPhases_PromiseFailuresNamePerTarget confirms a Promise
// failure is attributed to the target that produced it, and — unlike a Go
// failure — never sets promiseSkipped, so the summary reports elapsed times
// rather than "not run".
func TestRunVerifyTestPhases_PromiseFailuresNamePerTarget(t *testing.T) {
	fakeToolPath(t, "wasmtime", "node")
	var c verifyPhaseCounts
	s := stubSuites(&c, nil, nil, nil)
	s.promiseTests = func(_, target string) (string, error) {
		return "captured " + target, errors.New("tests failed")
	}
	res, err := runVerifyTestPhases(t.TempDir(), true, true, s)
	if err != nil {
		t.Fatalf("unexpected abort error: %v", err)
	}
	want := []string{"promise tests (host)", "promise tests (wasm32-wasi)", "promise tests (wasm32-web)"}
	if !slices.Equal(res.failures, want) {
		t.Errorf("failures = %v, want %v", res.failures, want)
	}
	if res.promiseSkipped {
		t.Error("promiseSkipped must stay false when the Promise phases actually ran")
	}
	// The captured output is what ExtractFailedSection re-parses for the
	// "Failed Tests" block — a failing phase must still hand it back.
	if res.hostOutput == "" || res.wasmOutput == "" || res.wasmWebOutput == "" {
		t.Errorf("output of a failing phase was dropped: %+v", res)
	}
}

// TestRunVerifyTestPhases_MissingWasmToolAborts covers the two hard aborts:
// asking for a target whose runner is not installed is an error, not a test
// failure, and it happens before that phase's suite is invoked.
func TestRunVerifyTestPhases_MissingWasmToolAborts(t *testing.T) {
	for _, tc := range []struct {
		name           string
		present        []string
		wasm, wasmWeb  bool
		wantErr        string
		wantPromiseRun int
	}{
		{"wasmtime", nil, true, false, "wasmtime not found", 1},
		{"node", []string{"wasmtime"}, true, true, "node not found", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeToolPath(t, tc.present...)
			var c verifyPhaseCounts
			res, err := runVerifyTestPhases(t.TempDir(), tc.wasm, tc.wasmWeb, stubSuites(&c, nil, nil, nil))
			if err == nil {
				t.Fatalf("expected an abort, got none (res %+v)", res)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if c.promiseN != tc.wantPromiseRun {
				t.Errorf("promise phases ran %d times, want %d (abort before the missing one)", c.promiseN, tc.wantPromiseRun)
			}
		})
	}
}

// TestRunVerifyTestPhases_AllGoSuitesFailListedTogether pins the other half of
// the abort rule: the three Go suites all run before the decision, so a run
// with several broken ones names them all rather than stopping at the first.
func TestRunVerifyTestPhases_AllGoSuitesFailListedTogether(t *testing.T) {
	var c verifyPhaseCounts
	boom := errors.New("boom")
	res, err := runVerifyTestPhases(t.TempDir(), false, false, stubSuites(&c, boom, boom, boom))
	if err != nil {
		t.Fatalf("unexpected abort error: %v", err)
	}
	want := []string{"go tests", "tools go tests", "flows go tests"}
	if !slices.Equal(res.failures, want) {
		t.Errorf("failures = %v, want %v", res.failures, want)
	}
	if c.promiseN != 0 {
		t.Errorf("promise tests ran %d times; want 0", c.promiseN)
	}
	// hostElapsed stays zero, which is exactly why the summary prints
	// "not run (go tests failed)" instead of a misleading "FAILED (0s)".
	if res.hostElapsed != 0 {
		t.Errorf("hostElapsed = %v, want 0 for a phase that never ran", res.hostElapsed)
	}
}

// TestRunVerifyTestPhases_SkippedFlowsIsNotAFailure covers the flows suite's
// third state: absent SDK reports skipped, and a skipped suite must neither be
// recorded as a failure nor stop the Promise phases.
func TestRunVerifyTestPhases_SkippedFlowsIsNotAFailure(t *testing.T) {
	var c verifyPhaseCounts
	s := stubSuites(&c, nil, nil, nil)
	s.flowsTests = func(string) (bool, error) { c.flowsN++; return true, errors.New("ignored when skipped") }
	res, err := runVerifyTestPhases(t.TempDir(), false, false, s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.flowsSkipped {
		t.Error("flowsSkipped should be reported so the summary can say why")
	}
	if len(res.failures) != 0 {
		t.Errorf("failures = %v, want none — a skipped suite is not a failure", res.failures)
	}
	if c.promiseN != 1 {
		t.Errorf("promise tests ran %d times; want 1", c.promiseN)
	}
	// flows/go.mod does not exist under a temp root, so the summary picks the
	// "flows/ absent" wording rather than "SDK absent".
	if res.flowsModPresent {
		t.Error("flowsModPresent should be false for a root with no flows/go.mod")
	}
}

// withInterruptAt runs the phase suites with the Ctrl+C flag raised by the
// named suite, so the phase runner's Interrupted() checkpoints can be exercised
// without a real signal. The flag is package-global, so it is always cleared.
func withInterruptAt(t *testing.T, s *verifySuites, at string) {
	t.Helper()
	t.Cleanup(func() { interrupted.Store(0) })
	raise := func() { interrupted.Store(1) }
	switch at {
	case "go":
		inner := s.goTests
		s.goTests = func(r string) error { raise(); return inner(r) }
	case "tools":
		inner := s.toolsTests
		s.toolsTests = func(r string) error { raise(); return inner(r) }
	case "flows":
		inner := s.flowsTests
		s.flowsTests = func(r string) (bool, error) { raise(); return inner(r) }
	case "promise":
		inner := s.promiseTests
		s.promiseTests = func(r, tgt string) (string, error) { raise(); return inner(r, tgt) }
	default:
		t.Fatalf("unknown interrupt point %q", at)
	}
}

// TestRunVerifyTestPhases_InterruptStopsAtNextCheckpoint covers all four Ctrl+C
// checkpoints. The pipeline stops between phases rather than mid-phase, so each
// one must return errInterrupted without starting the next suite.
func TestRunVerifyTestPhases_InterruptStopsAtNextCheckpoint(t *testing.T) {
	fakeToolPath(t, "wasmtime", "node")
	for _, tc := range []struct {
		at                                        string
		wantGo, wantTools, wantFlows, wantPromise int
	}{
		{"go", 1, 0, 0, 0},
		{"tools", 1, 1, 0, 0},
		{"flows", 1, 1, 1, 0},
		{"promise", 1, 1, 1, 1}, // host ran; wasm and wasm-web must not
	} {
		t.Run(tc.at, func(t *testing.T) {
			var c verifyPhaseCounts
			s := stubSuites(&c, nil, nil, nil)
			withInterruptAt(t, &s, tc.at)
			_, err := runVerifyTestPhases(t.TempDir(), true, true, s)
			if !errors.Is(err, errInterrupted) {
				t.Fatalf("err = %v, want errInterrupted", err)
			}
			if c.goN != tc.wantGo || c.toolsN != tc.wantTools ||
				c.flowsN != tc.wantFlows || c.promiseN != tc.wantPromise {
				t.Errorf("ran go/tools/flows/promise %d/%d/%d/%d, want %d/%d/%d/%d",
					c.goN, c.toolsN, c.flowsN, c.promiseN,
					tc.wantGo, tc.wantTools, tc.wantFlows, tc.wantPromise)
			}
		})
	}
}
