package common

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const cleanUsage = "usage: bin/clean [--local] [--quiet]"

// cleanTestHome points the user home — and the user cache directory under it,
// where the host verify lock lives — at a fresh temp directory for one test and
// returns it, so any ~/.promise or lock a clean or verify resolves is the
// test's own and the host's are never touched. os.UserCacheDir reads HOME on
// macOS, XDG_CACHE_HOME (else HOME) on Linux and LocalAppData on Windows, so
// all of them are redirected.
func cleanTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("LocalAppData", filepath.Join(home, "AppData", "Local"))
	return home
}

// cleanTestFile creates path, and every directory above it, holding placeholder
// content.
func cleanTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunClean_UnknownFlagReturnsUsageError is the wiring pin: parsing happens
// ahead of every side effect, so a mistyped flag returns the usage error
// without taking the verify lock or removing anything.
func TestRunClean_UnknownFlagReturnsUsageError(t *testing.T) {
	err := RunClean(t.TempDir(), []string{"--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
	if err.Error() != cleanUsage {
		t.Errorf("got %q, want %q", err.Error(), cleanUsage)
	}
}

// TestParseCleanArgs_EveryFlagSetsItsOption checks that every documented flag
// parses into the option it names, and into no other. It asserts on
// parseCleanArgs rather than running a clean (T2084).
func TestParseCleanArgs_EveryFlagSetsItsOption(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want CleanOptions
	}{
		{"local", []string{"--local"}, CleanOptions{}},
		{"quiet", []string{"--quiet"}, CleanOptions{Quiet: true}},
		{"local and quiet", []string{"--local", "--quiet"}, CleanOptions{Quiet: true}},
		{"single dash", []string{"-quiet"}, CleanOptions{Quiet: true}},
		{"none", nil, CleanOptions{}},
		{"repeated flag", []string{"--quiet", "--quiet"}, CleanOptions{Quiet: true}},
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
	if err.Error() != cleanUsage {
		t.Errorf("got %q, want %q", err.Error(), cleanUsage)
	}
	if got != (CleanOptions{}) {
		t.Errorf("a rejected command line must yield zero options, got %+v", got)
	}
}

// TestParseCleanArgs_SharedIsRefused: --shared used to clear the
// machine-global ~/.promise/cache. Nothing run from a worktree addresses that
// home any more (#102), so the flag is a usage error in either spelling — not
// a silent no-op that would leave its caller believing the shared cache was
// cleared.
func TestParseCleanArgs_SharedIsRefused(t *testing.T) {
	for _, args := range [][]string{{"--shared"}, {"-shared"}, {"--shared", "--quiet"}} {
		got, err := parseCleanArgs(args)
		if err == nil || err.Error() != cleanUsage {
			t.Errorf("parseCleanArgs(%v) = %+v, %v; want the usage error", args, got, err)
		}
	}
}

// TestCleanTarget pins what a clean removes: the worktree's own home, a pure
// function of the root. It does not consult the user home at all — with none
// set it still names the same directory — because the machine-global
// ~/.promise is not a worktree command's to clear (#102).
func TestCleanTarget(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // os.UserHomeDir on Windows
	root := filepath.Join(t.TempDir(), "some", "repo")

	if got, want := CleanTarget(root), filepath.Join(root, ".promise-home"); got != want {
		t.Errorf("CleanTarget = %q, want %q", got, want)
	}
}

// TestClean_AcquiresVerifyLock verifies the lock serialization that T0328
// requires: a caller holding the verify lock blocks any concurrent acquirer
// until the lock is released. This exercises acquireVerifyLockIn — the same
// mechanism Clean uses via acquireVerifyLock. Using a temp lock path avoids
// interference with real verify runs on the host verify lock.
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

// TestRunClean_SharedIsRefusedBeforeAnySideEffect drives the real entry point
// with --shared against a redirected HOME seeded like an installed machine.
// The flag is gone (#102): it is refused at parse time, so neither the shared
// home's cache nor the repo-local home is touched, and no lock is taken.
func TestRunClean_SharedIsRefusedBeforeAnySideEffect(t *testing.T) {
	home := cleanTestHome(t)
	sharedCache := filepath.Join(home, ".promise", "cache", "llvm-view", "opt")
	cleanTestFile(t, sharedCache)

	root := t.TempDir()
	localMarker := filepath.Join(root, ".promise-home", "keep")
	cleanTestFile(t, localMarker)

	err := RunClean(root, []string{"--shared"})
	if err == nil || err.Error() != cleanUsage {
		t.Fatalf("RunClean(--shared) = %v, want the usage error", err)
	}
	if !Exists(sharedCache) {
		t.Errorf("a refused clean removed %s", sharedCache)
	}
	if !Exists(localMarker) {
		t.Errorf("a refused clean removed %s", localMarker)
	}
}

// TestRunClean_LocalTouchesNothingShared is the default mode's end-to-end pin:
// it removes the whole repo-local home and writes no shared location. HOME and
// GOCACHE both point at directories this test owns, so a clean that reached for
// ~/.promise or ran `go clean -testcache` changes something here, where the
// assertions see it, instead of on the host.
func TestRunClean_LocalTouchesNothingShared(t *testing.T) {
	home := cleanTestHome(t)
	sharedMarker := filepath.Join(home, ".promise", "cache", "marker")
	cleanTestFile(t, sharedMarker)
	goCache := t.TempDir()
	t.Setenv("GOCACHE", goCache)

	root := t.TempDir()
	local := filepath.Join(root, ".promise-home")
	// tmp/ and cache/ both, to prove the clean takes everything under the home.
	for _, sub := range []string{"tmp/foo/x", "cache/llvm/marker", "cache/build/y"} {
		cleanTestFile(t, filepath.Join(local, filepath.FromSlash(sub)))
	}

	if err := RunClean(root, []string{"--quiet"}); err != nil {
		t.Fatalf("RunClean: %v", err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed, stat err = %v", local, err)
	}
	if !Exists(sharedMarker) {
		t.Errorf("a default clean must leave the shared home alone; %s is gone", sharedMarker)
	}
	if stamp := filepath.Join(goCache, "testexpire.txt"); Exists(stamp) {
		t.Errorf("a clean must not expire the Go test cache; %s was stamped", stamp)
	}
}

// TestRunClean_QuietReachesClean pins --quiet end to end: without it the clean
// names what it cleared, and with it the clean prints nothing.
func TestRunClean_QuietReachesClean(t *testing.T) {
	cleanTestHome(t) // the verify lock lands in this test's own home
	for _, tc := range []struct {
		name  string
		args  []string
		quiet bool
	}{
		{"default", nil, false},
		{"quiet", []string{"--quiet"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			local := filepath.Join(root, ".promise-home")
			cleanTestFile(t, filepath.Join(local, "x"))

			var err error
			out := captureStdout(t, func() { err = RunClean(root, tc.args) })
			if err != nil {
				t.Fatalf("RunClean(%v): %v", tc.args, err)
			}
			if Exists(local) {
				t.Errorf("the local home %s should have been cleaned", local)
			}
			if tc.quiet && strings.TrimSpace(out) != "" {
				t.Errorf("--quiet must print nothing, got:\n%s", out)
			}
			if !tc.quiet && !strings.Contains(out, local) {
				t.Errorf("a non-quiet clean must name the home it cleared; got:\n%s", out)
			}
		})
	}
}

// TestCleanLocked_RemoveAllError verifies cleanLocked reports a target it could
// not remove, naming it. The removal is blocked the way each platform allows: on
// Windows by holding a file open inside the target (an open file cannot be
// deleted there); elsewhere by making the target's parent read-only, which
// Windows ignores.
func TestCleanLocked_RemoveAllError(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, ".promise-home")
	held := filepath.Join(target, "held")
	cleanTestFile(t, held)

	if runtime.GOOS == "windows" {
		f, err := os.Open(held)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() }) // before t.TempDir removes root
	} else {
		if os.Getuid() == 0 {
			t.Skip("root can remove entries from read-only directories")
		}
		if err := os.Chmod(root, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	}

	err := cleanLocked(root, CleanOptions{Quiet: true})
	if err == nil {
		t.Fatal("expected error from cleanLocked when the target is unremovable, got nil")
	}
	if !strings.Contains(err.Error(), "remove "+target) {
		t.Errorf("the error must name the target it could not remove, got: %v", err)
	}
}

// TestClean_LockFailureCleansNothing covers Clean's own error branch, and the
// safety property behind it: the lock is taken before anything is removed, so a
// clean that cannot serialize itself removes nothing at all.
//
// The user dirs are redirected to a fresh temp directory first; the host's lock
// is never touched. Inside that temp home the test puts a regular file where the
// lock's directory belongs, so acquireVerifyLock cannot create verify.lock
// beneath it — on any platform, as any user. Making the directory read-only, the
// earlier induction, is ignored by Windows and by root, which then took the lock
// and cleaned.
func TestClean_LockFailureCleansNothing(t *testing.T) {
	cleanTestHome(t)
	lockPath, err := verifyLockPath()
	if err != nil {
		t.Fatal(err)
	}
	cleanTestFile(t, filepath.Dir(lockPath))

	root := t.TempDir()
	keep := filepath.Join(root, ".promise-home", "keep")
	cleanTestFile(t, keep)

	err = Clean(root, CleanOptions{Quiet: true})
	if err == nil {
		t.Fatal("Clean should fail when the verify lock cannot be taken")
	}
	if !strings.Contains(err.Error(), "acquire verify lock") {
		t.Errorf("the error must name the step that failed, got: %v", err)
	}
	if !Exists(keep) {
		t.Errorf("a clean that never took the lock must remove nothing; %s is gone", keep)
	}
}
