package common

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRunClean_UnknownFlagReturnsUsageError is the wiring pin: parsing happens
// ahead of every side effect, so a mistyped flag returns the usage error
// without taking the verify lock, removing a home, or running `go clean`.
func TestRunClean_UnknownFlagReturnsUsageError(t *testing.T) {
	err := RunClean(t.TempDir(), []string{"--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
	const want = "usage: bin/clean [--local|--shared] [--quiet]"
	if err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
}

// TestParseCleanArgs_EveryFlagSetsItsOption checks that every documented flag
// parses into the option it names, and into no other.
//
// It asserts on parseCleanArgs rather than running a clean (T2084). The two
// tests this replaces each proved a flag parsed the expensive way: one ran
// cleanLocked for real, the other re-implemented RunClean's switch inline —
// a second copy of the parser that could agree with the test while disagreeing
// with the tool.
func TestParseCleanArgs_EveryFlagSetsItsOption(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want CleanOptions
	}{
		{"local", []string{"--local"}, CleanOptions{}},
		{"shared", []string{"--shared"}, CleanOptions{Shared: true}},
		{"quiet", []string{"--quiet"}, CleanOptions{Quiet: true}},
		{"shared and quiet", []string{"--shared", "--quiet"}, CleanOptions{Shared: true, Quiet: true}},
		{"single dash", []string{"-shared"}, CleanOptions{Shared: true}},
		{"none", nil, CleanOptions{}},
		{"repeated flag", []string{"--quiet", "--quiet"}, CleanOptions{Quiet: true}},
		// --local is the default rather than an opposite: a later --local does
		// not undo an earlier --shared. Pinned because the usage string spells
		// them as alternatives, which reads like it would.
		{"local does not cancel shared", []string{"--shared", "--local"}, CleanOptions{Shared: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCleanArgs(tc.args)
			if err != nil {
				t.Fatalf("parseCleanArgs(%v) = %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseCleanArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// TestParseCleanArgs_UnknownFlagReturnsUsageError pins the rejection path at
// the parser, with the zero options a rejected command line must yield.
func TestParseCleanArgs_UnknownFlagReturnsUsageError(t *testing.T) {
	got, err := parseCleanArgs([]string{"--unknown"})
	if err == nil {
		t.Fatalf("parseCleanArgs = %+v, want an error", got)
	}
	const want = "usage: bin/clean [--local|--shared] [--quiet]"
	if err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
	if got != (CleanOptions{}) {
		t.Errorf("a rejected command line must yield zero options, got %+v", got)
	}
}

// TestCleanHome verifies the home-directory resolution for both --local and
// --shared modes: --local is rooted at the repo, --shared at the user's home.
//
// HOME is redirected to a directory this test names, so the shared answer is
// checked against a value chosen here rather than against os.UserHomeDir()
// recomputed — which would restate the implementation and pass either way. It
// also keeps the one test that asks for the shared home from naming the host's
// (T2084).
func TestCleanHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows

	root := filepath.Join(home, "some", "repo")
	got, err := CleanHome(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, ".promise-home"); got != want {
		t.Errorf("CleanHome(local) = %q, want %q", got, want)
	}

	gotShared, err := CleanHome(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if wantShared := filepath.Join(home, ".promise"); gotShared != wantShared {
		t.Errorf("CleanHome(shared) = %q, want %q", gotShared, wantShared)
	}
	if gotShared == got {
		t.Error("--shared and --local must resolve to different homes")
	}
}

// TestClean_AcquiresVerifyLock verifies the lock serialization that T0328
// requires: a caller holding the verify lock blocks any concurrent acquirer
// until the lock is released. This exercises acquireVerifyLockIn — the same
// mechanism Clean uses via acquireVerifyLock. Using a temp lock path avoids
// interference with real verify runs on the global ~/.promise/verify.lock.
func TestClean_AcquiresVerifyLock(t *testing.T) {
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "verify.lock")

	unlock, err := acquireVerifyLockIn(lockPath, "/holder", 0)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	cleanDone := make(chan struct{})
	go func() {
		defer wg.Done()
		inner, err := acquireVerifyLockIn(lockPath, "/waiter", 0)
		if err != nil {
			t.Errorf("acquireVerifyLockIn: %v", err)
			return
		}
		defer inner()
		close(cleanDone)
	}()

	// Goroutine must block while we hold the lock.
	select {
	case <-cleanDone:
		unlock()
		t.Fatal("goroutine acquired lock before it was released")
	case <-time.After(50 * time.Millisecond):
		// Good — blocked as expected.
	}

	unlock()

	select {
	case <-cleanDone:
		// Passed.
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine did not acquire lock within 5s after release")
	}

	wg.Wait()
}

// TestCleanLocked_SharedTargetsTheUserHome covers the Shared branch in the one
// way it can be covered safely: with HOME redirected, so the home cleanLocked
// resolves and removes is this test's own (T2084). It pins both halves of the
// choice — the shared home goes, the repo-local one is left alone.
func TestCleanLocked_SharedTargetsTheUserHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	silenceGoTelemetry(t, home)
	isolateTestCache(t)

	root := newCleanRoot(t)
	sharedHome := filepath.Join(home, ".promise")
	if err := os.MkdirAll(filepath.Join(sharedHome, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sharedHome, "bin", "promise"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	localHome := filepath.Join(root, ".promise-home")
	if err := os.MkdirAll(localHome, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := cleanLocked(root, CleanOptions{Shared: true, Quiet: true}); err != nil {
		t.Fatalf("cleanLocked: %v", err)
	}

	if _, err := os.Stat(sharedHome); !os.IsNotExist(err) {
		t.Errorf("expected the shared home %s to be removed, stat err = %v", sharedHome, err)
	}
	if !Exists(localHome) {
		t.Errorf("--shared must not touch the repo-local home %s", localHome)
	}
}

// TestCleanLocked_RemoveAllError verifies cleanLocked returns an error when
// os.RemoveAll fails (e.g., permission denied on the home directory).
func TestCleanLocked_RemoveAllError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can remove read-only directories")
	}
	isolateTestCache(t) // RemoveAll fails first today; one reordering away from running `go clean`
	root := t.TempDir()
	home := filepath.Join(root, ".promise-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	// Make the home directory itself unwritable so RemoveAll fails.
	parent := filepath.Dir(home)
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0o755) //nolint:errcheck

	err := cleanLocked(root, CleanOptions{Quiet: true})
	if err == nil {
		t.Fatal("expected error from cleanLocked when home is unremovable, got nil")
	}
}

// isolateTestCache points GOCACHE at a throwaway directory for the duration of
// one test, and returns it.
//
// Every cleanLocked runs `go clean -testcache`, which is host-global: it stamps
// $GOCACHE/testexpire.txt, and from that moment cmd/go treats every test result
// saved earlier as expired — in every module, and in every clone sharing the
// cache. The throwaway compiler/go.mod under the temp root does not scope it;
// only GOCACHE does. Before T2084 these tests expired the compiler results
// bin/verify had saved minutes earlier, charging the next verify a full ~4-minute
// rerun of all 54 compiler packages.
func isolateTestCache(t *testing.T) string {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("GOCACHE", cache)
	return cache
}

// silenceGoTelemetry stops the `go` command from writing telemetry counters
// under a redirected HOME.
//
// A background sidecar writes those counters *after* go exits, so they can
// reappear inside a t.TempDir() while its cleanup is removing it — the cleanup
// then fails with "directory not empty". Pre-seeding the mode file with "off"
// keeps the redirected home to exactly what the test put there. Where the
// config directory is not under HOME at all (Windows uses %AppData%), there is
// nothing to seed and nothing lands in the sandbox.
func silenceGoTelemetry(t *testing.T, home string) {
	t.Helper()
	cfg, err := os.UserConfigDir()
	if err != nil || !strings.HasPrefix(filepath.Clean(cfg), filepath.Clean(home)+string(filepath.Separator)) {
		return
	}
	dir := filepath.Join(cfg, "go", "telemetry")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mode"), []byte("off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newCleanRoot returns a temp root holding the minimal compiler/go.mod that
// cleanLocked's `go clean -testcache` step needs to run in.
func newCleanRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	compilerDir := filepath.Join(root, "compiler")
	if err := os.MkdirAll(compilerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compilerDir, "go.mod"), []byte("module testmod\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestRunClean_EndToEnd verifies that cleanLocked succeeds for the --local
// case. We call cleanLocked directly (bypassing lock acquisition) to avoid
// blocking on the global verify lock when this test runs inside bin/verify —
// the lock behaviour is already covered by TestClean_AcquiresVerifyLock.
//
// The expiry stamp is asserted to land in this test's own GOCACHE, which is
// what keeps the isolation honest: the assertion reddens both if the stamp
// escapes to the host cache and if the `go clean` step silently stops running.
func TestRunClean_EndToEnd(t *testing.T) {
	cache := isolateTestCache(t)
	root := newCleanRoot(t)
	if err := cleanLocked(root, CleanOptions{Quiet: true}); err != nil {
		t.Fatalf("cleanLocked: %v", err)
	}
	stamp := filepath.Join(cache, "testexpire.txt")
	if !Exists(stamp) {
		t.Errorf("go clean -testcache should have stamped %s — the test cache it expired was some other one", stamp)
	}
}

// TestClean_RemovesLocalHome verifies that cleanLocked wipes the entire
// .promise-home directory tree, not just tmp/. We call cleanLocked directly
// (bypassing lock acquisition) to avoid blocking on the global verify lock
// when this test runs inside bin/verify — the lock behaviour is already
// covered by TestClean_AcquiresVerifyLock.
func TestClean_RemovesLocalHome(t *testing.T) {
	isolateTestCache(t)
	root := newCleanRoot(t)
	home := filepath.Join(root, ".promise-home")

	// Populate a fake home with both tmp/ and cache/ subdirs to prove that
	// cleanLocked nukes everything under the home (not just tmp).
	for _, sub := range []string{"tmp", "cache/llvm", "cache/build", "tmp/foo"} {
		if err := os.MkdirAll(filepath.Join(home, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "cache", "llvm", "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cleanLocked(root, CleanOptions{Quiet: true}); err != nil {
		t.Fatalf("cleanLocked: %v", err)
	}

	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed, stat err = %v", home, err)
	}
}

// TestRunClean_SharedReachesCleanWithTheParsedOptions is the other half of the
// split: TestRunClean_UnknownFlagReturnsUsageError pins that a rejected command
// line stops at the parser, and this pins that an accepted one is carried
// through to the work — lock included — as the options it parsed into.
//
// Splitting RunClean into parse + act made a silent regression possible that
// the parser tests cannot see: forwarding CleanOptions{} instead of opts would
// leave every parse test green while `bin/clean --shared` quietly cleaned the
// repo-local home. So this drives the real entry point with the one flag that
// changes the target, and names both outcomes — the shared home goes, the
// repo-local one does not.
//
// HOME is redirected, so the "shared" home is this test's own; without that
// this is the exact call that deleted the installed toolchain (T2084).
func TestRunClean_SharedReachesCleanWithTheParsedOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	silenceGoTelemetry(t, home)
	isolateTestCache(t)

	root := newCleanRoot(t)
	sharedHome := filepath.Join(home, ".promise")
	if err := os.MkdirAll(filepath.Join(sharedHome, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	localHome := filepath.Join(root, ".promise-home")
	if err := os.MkdirAll(localHome, 0o755); err != nil {
		t.Fatal(err)
	}

	// No --quiet: the default invocation is the one an operator runs, and after
	// T2084 the path it prints is the only record of which home a clean took.
	var err error
	out := captureStdout(t, func() { err = RunClean(root, []string{"--shared"}) })
	if err != nil {
		t.Fatalf("RunClean: %v", err)
	}

	if Exists(sharedHome) {
		t.Errorf("--shared must clean %s", sharedHome)
	}
	if !Exists(localHome) {
		t.Errorf("--shared must not touch the repo-local home %s", localHome)
	}
	if !strings.Contains(out, sharedHome) {
		t.Errorf("a non-quiet clean must name the home it cleared; got:\n%s", out)
	}
}

// TestRunClean_QuietReachesCleanSilently pins the other parsed option end to
// end: --quiet must reach cleanLocked's logger, not merely set a field. It runs
// local (the default), so the home it removes is inside the temp root.
func TestRunClean_QuietReachesCleanSilently(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	silenceGoTelemetry(t, home)
	isolateTestCache(t)

	root := newCleanRoot(t)
	localHome := filepath.Join(root, ".promise-home")
	if err := os.MkdirAll(localHome, 0o755); err != nil {
		t.Fatal(err)
	}
	// The shared home exists too, and must come through untouched: the verify
	// lock lives inside it, so a default clean does open it — it just must not
	// clean it.
	sharedCache := filepath.Join(home, ".promise", "cache")
	if err := os.MkdirAll(sharedCache, 0o755); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStdout(t, func() { err = RunClean(root, []string{"--quiet"}) })
	if err != nil {
		t.Fatalf("RunClean: %v", err)
	}
	if Exists(localHome) {
		t.Errorf("the local home %s should have been cleaned", localHome)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("--quiet must print nothing, got:\n%s", out)
	}
	if !Exists(sharedCache) {
		t.Errorf("a default (local) clean must leave the shared home alone; %s is gone", sharedCache)
	}
}

// TestCleanHome_UnresolvableHomeIsAnError covers the one branch of CleanHome
// that does not return a path.
//
// It matters more than its size suggests: the value being computed is the
// argument to os.RemoveAll. A CleanHome that swallowed the error and returned
// filepath.Join("", ".promise") would hand RemoveAll the relative path
// ".promise" — deleting whatever that names in the process's working
// directory. So the contract is that an unnameable home fails the clean rather
// than aiming it somewhere else, and cleanLocked must surface that failure
// having removed nothing.
func TestCleanHome_UnresolvableHomeIsAnError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // os.UserHomeDir on Windows
	isolateTestCache(t)

	root := t.TempDir()
	sentinel := filepath.Join(root, ".promise-home", "keep")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got, err := CleanHome(root, true); err == nil {
		t.Errorf("CleanHome(shared) with no home = %q, want an error", got)
	}
	// The local answer needs no home, so it must still be available.
	got, err := CleanHome(root, false)
	if err != nil {
		t.Fatalf("CleanHome(local) must not depend on the user home: %v", err)
	}
	if want := filepath.Join(root, ".promise-home"); got != want {
		t.Errorf("CleanHome(local) = %q, want %q", got, want)
	}

	err = cleanLocked(root, CleanOptions{Shared: true, Quiet: true})
	if err == nil {
		t.Fatal("cleanLocked(shared) with no home should fail")
	}
	if !strings.Contains(err.Error(), "resolve promise home") {
		t.Errorf("the error must name the step that failed, got: %v", err)
	}
	if !Exists(sentinel) {
		t.Error("a clean that could not name its target must remove nothing")
	}
}

// TestCleanLocked_GoCleanFailureIsReported covers the second half of a clean:
// the `go clean -testcache` step, which fails here because the root has no
// compiler/ directory to run it in.
//
// It also pins the ordering an operator sees after a half-finished clean — the
// home is already gone when the error is returned — so a repeat run is the
// expected recovery, not a sign the first one did nothing.
func TestCleanLocked_GoCleanFailureIsReported(t *testing.T) {
	isolateTestCache(t)

	root := t.TempDir() // deliberately no compiler/go.mod
	home := filepath.Join(root, ".promise-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}

	err := cleanLocked(root, CleanOptions{Quiet: true})
	if err == nil {
		t.Fatal("cleanLocked should fail when `go clean` cannot run")
	}
	if !strings.Contains(err.Error(), "go clean -testcache") {
		t.Errorf("the error must name the step that failed, got: %v", err)
	}
	if Exists(home) {
		t.Error("the home is removed before the go clean step; a failure there leaves it removed")
	}
}

// TestClean_LockFailureCleansNothing covers Clean's own error branch, and the
// safety property behind it: the lock is taken before anything is removed, so a
// clean that cannot serialize itself removes nothing at all.
//
// The failure is induced by making the directory the lock lives in
// unwritable — HOME is redirected first, so the unwritable directory is this
// test's own.
func TestClean_LockFailureCleansNothing(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write to read-only directories")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	isolateTestCache(t)

	// 0o500: acquireVerifyLock cannot create ~/.promise/verify.lock here.
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) }) // so t.TempDir can remove it

	root := newCleanRoot(t)
	localHome := filepath.Join(root, ".promise-home")
	if err := os.MkdirAll(localHome, 0o755); err != nil {
		t.Fatal(err)
	}

	err := Clean(root, CleanOptions{Quiet: true})
	if err == nil {
		t.Fatal("Clean should fail when the verify lock cannot be taken")
	}
	if !strings.Contains(err.Error(), "acquire verify lock") {
		t.Errorf("the error must name the step that failed, got: %v", err)
	}
	if !Exists(localHome) {
		t.Errorf("a clean that never took the lock must remove nothing; %s is gone", localHome)
	}
}
