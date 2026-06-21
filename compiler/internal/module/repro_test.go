package module

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestResolveRemoteModuleSurvivesUpstreamDeletion is the §9.10 / §6.2
// reproducibility guarantee: once a commit is fetched into the local cache, the
// module keeps resolving even if the upstream repo is later deleted. A project
// that already builds keeps building forever, independent of ecosystem churn.
func TestResolveRemoteModuleSurvivesUpstreamDeletion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	bareRepo, commitHash := createTestRepo(t, "survivor")

	t.Setenv("HOME", t.TempDir())
	os.Unsetenv("PROMISE_HOME")

	// First resolve fetches + checks out into the cache.
	dir1, err := ResolveRemoteModule(bareRepo, commitHash)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir1, "promise.toml")); err != nil {
		t.Fatal("expected promise.toml in resolved checkout")
	}

	// Upstream vanishes — the repo the team does not own is deleted.
	if err := os.RemoveAll(bareRepo); err != nil {
		t.Fatal(err)
	}

	// Re-resolve must still succeed from the cache (fast path: checkout exists).
	dir2, err := ResolveRemoteModule(bareRepo, commitHash)
	if err != nil {
		t.Fatalf("resolve after upstream deletion: %v", err)
	}
	if dir1 != dir2 {
		t.Errorf("cache path changed: %q vs %q", dir1, dir2)
	}
	if _, err := os.Stat(filepath.Join(dir2, "survivor.pr")); err != nil {
		t.Fatal("expected survivor.pr to still be available from cache")
	}
}
