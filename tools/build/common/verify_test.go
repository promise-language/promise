package common

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquireVerifyLock_WritesRepoDir(t *testing.T) {
	// Override lock dir to a temp directory so we don't conflict with real runs.
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "verify.lock")

	unlock, err := acquireVerifyLockIn(lockPath, "/home/user/my-repo")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	// Lock file should contain the repo directory.
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(data))
	if got != "/home/user/my-repo" {
		t.Errorf("lock file = %q, want %q", got, "/home/user/my-repo")
	}
}

func TestAcquireVerifyLock_ClearsOnUnlock(t *testing.T) {
	lockDir := t.TempDir()
	lockPath := filepath.Join(lockDir, "verify.lock")

	unlock, err := acquireVerifyLockIn(lockPath, "/home/user/my-repo")
	if err != nil {
		t.Fatal(err)
	}

	// Unlock should clear the file.
	unlock()

	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Errorf("lock file after unlock = %q, want empty", string(data))
	}
}
