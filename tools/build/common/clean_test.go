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

const cleanUsage = "usage: bin/clean [--local|--shared] [--quiet]"

// cleanTestHome points the user home at a fresh temp directory for one test and
// returns it, so the ~/.promise any clean or lock resolves is the test's own and
// the real one is never touched.
func cleanTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
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
	if err.Error() != cleanUsage {
		t.Errorf("got %q, want %q", err.Error(), cleanUsage)
	}
	if got != (CleanOptions{}) {
		t.Errorf("a rejected command line must yield zero options, got %+v", got)
	}
}

// TestCleanTarget pins what each mode removes: the repo-local home, or the
// shared home's cache subtree — never the shared home itself, which is also the
// install root and the verify lock's directory. HOME is redirected, so the
// shared answer is checked against a value chosen here.
func TestCleanTarget(t *testing.T) {
	home := cleanTestHome(t)
	root := filepath.Join(home, "some", "repo")

	local, err := CleanTarget(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, ".promise-home"); local != want {
		t.Errorf("CleanTarget(local) = %q, want %q", local, want)
	}

	shared, err := CleanTarget(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".promise", "cache"); shared != want {
		t.Errorf("CleanTarget(shared) = %q, want %q", shared, want)
	}
	if shared == filepath.Join(home, ".promise") {
		t.Error("--shared must never target ~/.promise itself — it holds the installed toolchain and the verify lock")
	}
}

// TestCleanTarget_UnresolvableHomeIsAnError covers the one branch of CleanTarget
// that does not return a path. The value being computed is the argument to
// os.RemoveAll: a CleanTarget that swallowed the error and returned a relative
// ".promise/cache" would delete whatever that names in the working directory.
// So an unnameable home fails the clean, having removed nothing.
func TestCleanTarget_UnresolvableHomeIsAnError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // os.UserHomeDir on Windows

	root := t.TempDir()
	sentinel := filepath.Join(root, ".promise-home", "keep")
	cleanTestFile(t, sentinel)

	if got, err := CleanTarget(root, true); err == nil {
		t.Errorf("CleanTarget(shared) with no home = %q, want an error", got)
	}
	if _, err := CleanTarget(root, false); err != nil {
		t.Fatalf("CleanTarget(local) must not depend on the user home: %v", err)
	}

	err := cleanLocked(root, CleanOptions{Shared: true, Quiet: true})
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

// TestRunClean_SharedClearsOnlyTheCache drives the real entry point with
// --shared against a redirected HOME seeded like an installed machine, and pins
// both bugs this mode used to have:
//
//   - T2095: Clean holds ~/.promise/verify.lock open while it cleans. When the
//     target was ~/.promise itself, Windows refused to delete the open lock and
//     the clean failed. The lock now sits outside the target, so the clean must
//     succeed on every platform.
//   - T1925: ~/.promise is also the install root. epochs/, bin/ and active are
//     the installed toolchain and must survive; only cache/ goes.
//
// It also pins that the options reach the work as parsed — the repo-local home
// is left alone — and that nothing stamps the Go test cache.
func TestRunClean_SharedClearsOnlyTheCache(t *testing.T) {
	home := cleanTestHome(t)
	goCache := t.TempDir()
	t.Setenv("GOCACHE", goCache)

	shared := filepath.Join(home, ".promise")
	cache := filepath.Join(shared, "cache")
	cleanTestFile(t, filepath.Join(cache, "llvm-view", "opt"))
	cleanTestFile(t, filepath.Join(cache, "blobs", "abc"))
	installed := []string{
		filepath.Join(shared, "epochs", "2026.9", "bin", "promise"),
		filepath.Join(shared, "bin", "promise"),
		filepath.Join(shared, "active"),
	}
	for _, f := range installed {
		cleanTestFile(t, f)
	}

	root := t.TempDir()
	localMarker := filepath.Join(root, ".promise-home", "keep")
	cleanTestFile(t, localMarker)

	// No --quiet: the path it prints is the only record of what a clean took.
	var err error
	out := captureStdout(t, func() { err = RunClean(root, []string{"--shared"}) })
	if err != nil {
		t.Fatalf("RunClean(--shared): %v", err)
	}

	if Exists(cache) {
		t.Errorf("--shared must clear %s", cache)
	}
	for _, f := range installed {
		if !Exists(f) {
			t.Errorf("--shared must leave the installed toolchain alone; %s is gone", f)
		}
	}
	if !Exists(localMarker) {
		t.Errorf("--shared must not touch the repo-local home; %s is gone", localMarker)
	}
	if !strings.Contains(out, cache) {
		t.Errorf("a non-quiet clean must name what it cleared; got:\n%s", out)
	}
	if stamp := filepath.Join(goCache, "testexpire.txt"); Exists(stamp) {
		t.Errorf("a clean must not expire the Go test cache; %s was stamped", stamp)
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
// HOME is redirected to a fresh temp directory first; the real ~/.promise is
// never touched. Inside that temp home the test puts a regular file named
// .promise where the lock's directory belongs, so acquireVerifyLock cannot
// create .promise/verify.lock beneath it — on any platform, as any user. Making
// the directory read-only, the earlier induction, is ignored by Windows and by
// root, which then took the lock and cleaned.
func TestClean_LockFailureCleansNothing(t *testing.T) {
	home := cleanTestHome(t)
	if err := os.WriteFile(filepath.Join(home, ".promise"), []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	keep := filepath.Join(root, ".promise-home", "keep")
	cleanTestFile(t, keep)

	err := Clean(root, CleanOptions{Quiet: true})
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
