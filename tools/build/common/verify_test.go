package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunVerify_UnknownFlagReturnsUsageError verifies that passing an unknown
// flag causes RunVerify to return the usage error immediately (before any lock
// or filesystem side effects). Also confirms the usage string includes [--push].
func TestRunVerify_UnknownFlagReturnsUsageError(t *testing.T) {
	err := RunVerify(t.TempDir(), []string{"--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
	const want = "usage: bin/verify [--shared] [--wasm] [--wasm-web] [--clean] [--push]"
	if err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
}

// TestRunVerify_PushFlagIsValid verifies that --push is accepted by the arg
// validation switch and does not produce a usage error.
// --shared is included to skip SetupLocalCache env-var side effects.
// The pipeline will fail in a temp dir (no source code), but the error must
// not be the usage-validation error.
func TestRunVerify_PushFlagIsValid(t *testing.T) {
	err := RunVerify(t.TempDir(), []string{"--shared", "--push"})
	if err != nil && strings.HasPrefix(err.Error(), "usage:") {
		t.Errorf("--push treated as unknown flag, got usage error: %v", err)
	}
}

// TestRunVerify_AllKnownFlagsAreValid checks that every documented flag
// individually passes arg validation.
func TestRunVerify_AllKnownFlagsAreValid(t *testing.T) {
	for _, flag := range []string{"--local", "--shared", "--wasm", "--wasm-web", "--clean", "--push"} {
		// Always pair with --shared to avoid SetupLocalCache env-var side effects.
		args := []string{"--shared", flag}
		if flag == "--shared" {
			args = []string{"--shared"}
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

	unlock, err := acquireVerifyLockIn(lockPath, "/home/user/my-repo")
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

func TestAcquireVerifyLock_ClearsOnUnlock(t *testing.T) {
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "verify.lock")

	unlock, err := acquireVerifyLockIn(lockPath, "/home/user/my-repo")
	if err != nil {
		t.Fatal(err)
	}

	// Unlock should clear the holder metadata.
	unlock()

	if _, err := os.Stat(lockPath + ".owner"); !os.IsNotExist(err) {
		t.Errorf("owner file should be removed after unlock, stat err = %v", err)
	}
}
