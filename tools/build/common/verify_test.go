package common

import (
	"errors"
	"os"
	"path/filepath"
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
