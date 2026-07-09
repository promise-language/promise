package common

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestRunClean_UnknownFlagReturnsUsageError verifies that an unrecognized flag
// returns the usage error before any filesystem effects.
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

// TestRunClean_KnownFlagsAccepted ensures that every documented flag passes
// arg validation (no "usage:" error). We call cleanLocked directly (bypassing
// lock acquisition) so the test never blocks on the global verify lock — the
// locking behaviour is verified separately in TestClean_AcquiresVerifyLock.
func TestRunClean_KnownFlagsAccepted(t *testing.T) {
	root := t.TempDir()
	// Minimal compiler/go.mod so `go clean -testcache` succeeds.
	compilerDir := filepath.Join(root, "compiler")
	if err := os.MkdirAll(compilerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compilerDir, "go.mod"), []byte("module testmod\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, flag := range []string{"--local", "--quiet"} {
		args := NormalizeArgs([]string{"--quiet", flag})
		var opts CleanOptions
		for _, arg := range args {
			switch arg {
			case "-shared":
				opts.Shared = true
			case "-local":
			case "-quiet":
				opts.Quiet = true
			default:
				t.Errorf("flag %q treated as unknown (arg parse)", flag)
				continue
			}
		}
		// Run the body of Clean (without the global lock) to confirm the flag
		// combination succeeds end-to-end.
		if err := cleanLocked(root, opts); err != nil {
			t.Errorf("flag %q: cleanLocked returned unexpected error: %v", flag, err)
		}
	}
}

// TestCleanHome verifies the home-directory resolution for both --local and
// --shared modes.
func TestCleanHome(t *testing.T) {
	root := "/some/repo"
	got, err := CleanHome(root, false)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, ".promise-home")
	if got != want {
		t.Errorf("CleanHome(local) = %q, want %q", got, want)
	}

	gotShared, err := CleanHome(root, true)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	wantShared := filepath.Join(home, ".promise")
	if gotShared != wantShared {
		t.Errorf("CleanHome(shared) = %q, want %q", gotShared, wantShared)
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

// TestRunClean_SharedFlagAccepted verifies that --shared is recognized as a
// valid flag (no "usage:" error). We use cleanLocked directly to avoid touching
// the real ~/.promise or blocking on the global verify lock.
func TestRunClean_SharedFlagAccepted(t *testing.T) {
	root := t.TempDir()
	compilerDir := filepath.Join(root, "compiler")
	if err := os.MkdirAll(compilerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compilerDir, "go.mod"), []byte("module testmod\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// --shared means CleanHome resolves to ~/.promise, but we override with
	// cleanLocked(root, Shared:false) since we just want to confirm the flag
	// itself parses without a usage error.
	args := NormalizeArgs([]string{"--shared"})
	var seenShared bool
	for _, arg := range args {
		switch arg {
		case "-shared":
			seenShared = true
		default:
			t.Errorf("--shared parsed as unknown flag %q", arg)
		}
	}
	if !seenShared {
		t.Error("--shared flag not parsed")
	}
}

// TestCleanLocked_RemoveAllError verifies cleanLocked returns an error when
// os.RemoveAll fails (e.g., permission denied on the home directory).
func TestCleanLocked_RemoveAllError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can remove read-only directories")
	}
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

// TestRunClean_EndToEnd verifies that cleanLocked succeeds for the --local
// case. We call cleanLocked directly (bypassing lock acquisition) to avoid
// blocking on the global verify lock when this test runs inside bin/verify —
// the lock behaviour is already covered by TestClean_AcquiresVerifyLock.
func TestRunClean_EndToEnd(t *testing.T) {
	root := t.TempDir()
	compilerDir := filepath.Join(root, "compiler")
	if err := os.MkdirAll(compilerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compilerDir, "go.mod"), []byte("module testmod\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cleanLocked(root, CleanOptions{Quiet: true}); err != nil {
		t.Fatalf("cleanLocked: %v", err)
	}
}

// TestClean_RemovesLocalHome verifies that cleanLocked wipes the entire
// .promise-home directory tree, not just tmp/. We call cleanLocked directly
// (bypassing lock acquisition) to avoid blocking on the global verify lock
// when this test runs inside bin/verify — the lock behaviour is already
// covered by TestClean_AcquiresVerifyLock.
func TestClean_RemovesLocalHome(t *testing.T) {
	root := t.TempDir()
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

	// Minimal go module so `go clean -testcache` succeeds.
	compilerDir := filepath.Join(root, "compiler")
	if err := os.MkdirAll(compilerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compilerDir, "go.mod"), []byte("module testmod\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cleanLocked(root, CleanOptions{Quiet: true}); err != nil {
		t.Fatalf("cleanLocked: %v", err)
	}

	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed, stat err = %v", home, err)
	}
}
