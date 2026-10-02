package common

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSetupLocalCacheClearsInheritedPromiseCache: a worktree command keeps its
// whole Promise cache in .promise-home, so a PROMISE_CACHE inherited from the
// caller (forge sets one for every child) must not split it across two roots.
func TestSetupLocalCacheClearsInheritedPromiseCache(t *testing.T) {
	// t.Setenv registers restoration of all three, which SetupLocalCache mutates.
	t.Setenv("PROMISE_HOME", "")
	t.Setenv("TMPDIR", os.Getenv("TMPDIR"))
	t.Setenv("PROMISE_CACHE", filepath.Join(t.TempDir(), "inherited"))

	root := t.TempDir()
	if err := SetupLocalCache(root); err != nil {
		t.Fatalf("SetupLocalCache: %v", err)
	}
	if v, ok := os.LookupEnv("PROMISE_CACHE"); ok {
		t.Errorf("PROMISE_CACHE = %q after SetupLocalCache, want unset", v)
	}
	if got, want := os.Getenv("PROMISE_HOME"), filepath.Join(root, ".promise-home"); got != want {
		t.Errorf("PROMISE_HOME = %q, want %q", got, want)
	}
}
