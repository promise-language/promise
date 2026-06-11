package common

import (
	"os"
	"os/exec"
	"testing"
)

// initGitRepo creates an empty git repo at dir with one commit, returning the
// full HEAD SHA. Shared by the provenance-stamping tests (T0854).
func initGitRepo(t *testing.T, dir string) string {
	t.Helper()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.name", "test")
	git("config", "user.email", "test@test")
	if err := os.WriteFile(dir+"/seed.txt", []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "seed.txt")
	git("commit", "-m", "seed")

	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return string(out[:len(out)-1]) // strip trailing newline
}

// TestGitSHAFull: the function returns HEAD's full 40-char hex SHA in a git repo
// (matching `git rev-parse HEAD`), and the SHA must satisfy isFullGitSHA so the
// install gate accepts it as valid provenance (T0854).
func TestGitSHAFull(t *testing.T) {
	dir := t.TempDir()
	want := initGitRepo(t, dir)

	got := GitSHAFull(dir)
	if got != want {
		t.Errorf("GitSHAFull = %q, want %q", got, want)
	}
	if len(got) != 40 {
		t.Errorf("GitSHAFull returned %d chars, want 40: %q", len(got), got)
	}
	// The stamp must be a form the install gate's provenance check accepts.
	if !isFullGitSHA(got) {
		t.Errorf("isFullGitSHA(%q) = false; install gate would reject a real HEAD SHA", got)
	}
}

// TestGitSHAFull_NotARepo: outside a git repo GitSHAFull returns "" (not the
// "unknown" sentinel of the short GitSHA), so the install gate treats a binary
// built without git provenance as "no provenance" rather than feeding a sentinel
// to git cat-file (T0854).
func TestGitSHAFull_NotARepo(t *testing.T) {
	dir := t.TempDir() // empty, no `git init`
	if got := GitSHAFull(dir); got != "" {
		t.Errorf("GitSHAFull(non-repo) = %q, want \"\"", got)
	}
}
