package module

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// createTestRepo creates a bare git repo with a promise.toml and a .pr file,
// then returns the bare repo path and the commit hash.
func createTestRepo(t *testing.T, name string) (bareRepo, commitHash string) {
	t.Helper()

	// Create a normal repo, commit files, then clone as bare
	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}

	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v failed: %s\n%s", args, err, out)
		}
		return string(out)
	}

	run(workDir, "git", "init", "--initial-branch=main")
	run(workDir, "git", "config", "user.email", "test@test.com")
	run(workDir, "git", "config", "user.name", "Test")

	// Write promise.toml
	toml := "[module]\nname = \"" + name + "\"\nepoch = \"2026.0\"\n"
	os.WriteFile(filepath.Join(workDir, "promise.toml"), []byte(toml), 0644)

	// Write a .pr file
	pr := name + "_hello() int `public { return 42; }\n"
	os.WriteFile(filepath.Join(workDir, name+".pr"), []byte(pr), 0644)

	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "initial")

	// Get the commit hash
	hash := run(workDir, "git", "rev-parse", "HEAD")
	hash = hash[:len(hash)-1] // strip newline

	// Clone as bare repo
	bareRepo = filepath.Join(t.TempDir(), name+".git")
	run("", "git", "clone", "--bare", "--quiet", workDir, bareRepo)

	return bareRepo, hash
}

func TestIsFullCommitHash(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", true},
		{"0000000000000000000000000000000000000000", true},
		{"abcdef", false}, // too short
		{"ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ", false}, // not hex
		{"", false},
		{"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b", false},   // 39 chars
		{"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2x", false}, // 41 chars
	}
	for _, tt := range tests {
		got := IsFullCommitHash(tt.input)
		if got != tt.want {
			t.Errorf("IsFullCommitHash(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestIsCommitHashLike(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"a1b2c3d", true},
		{"a1b2c3d4e5f6", true},
		{"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", true}, // 40 chars
		{"abc", false},     // too short
		{"xyz1234", false}, // not hex
		{"", false},
	}
	for _, tt := range tests {
		got := IsCommitHashLike(tt.input)
		if got != tt.want {
			t.Errorf("IsCommitHashLike(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestURLToCachePath(t *testing.T) {
	tests := []struct {
		cacheDir string
		url      string
		want     string
	}{
		{"/home/user/.promise/cache/modules", "github.com/someone/parser", "/home/user/.promise/cache/modules/github.com/someone/parser"},
		{"/cache", "git.corp.com/team/utils", "/cache/git.corp.com/team/utils"},
		// Colons are illegal in a Windows path component, so scp-style SSH URLs and
		// local drive-letter paths (both keep a ':' after NormalizeURL) are sanitized
		// to '_' — otherwise MkdirAll rejects the cache path on Windows. The '/'
		// separators that mark cache directories are preserved.
		{"/cache", "git@github.com:user/repo", "/cache/git@github.com_user/repo"},
		{"/cache", "c:/Users/dev/mymod", "/cache/c_/Users/dev/mymod"},
	}
	for _, tt := range tests {
		got := filepath.ToSlash(URLToCachePath(tt.cacheDir, tt.url))
		if got != tt.want {
			t.Errorf("URLToCachePath(%q, %q) = %q, want %q", tt.cacheDir, tt.url, got, tt.want)
		}
	}
}

func TestGlobalCacheDir(t *testing.T) {
	// Just verify it returns a non-empty string and the dir exists
	dir, err := GlobalCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir == "" {
		t.Fatal("expected non-empty cache dir")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("cache dir doesn't exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("cache dir is not a directory")
	}
}

func TestEnsureBareRepoAndCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	bareRepo, commitHash := createTestRepo(t, "testmod")

	// Test ensureBareRepo with a fresh clone
	cloneDir := filepath.Join(t.TempDir(), "clone.git")
	err := ensureBareRepo(cloneDir, bareRepo)
	if err != nil {
		t.Fatalf("ensureBareRepo (clone): %v", err)
	}

	// Verify HEAD exists in the clone
	if _, err := os.Stat(filepath.Join(cloneDir, "HEAD")); err != nil {
		t.Fatal("expected HEAD in cloned bare repo")
	}

	// Test ensureBareRepo again (fetch path)
	err = ensureBareRepo(cloneDir, bareRepo)
	if err != nil {
		t.Fatalf("ensureBareRepo (fetch): %v", err)
	}

	// Test ensureCheckout
	checkoutDir := filepath.Join(t.TempDir(), "checkout")
	err = ensureCheckout(cloneDir, checkoutDir, commitHash)
	if err != nil {
		t.Fatalf("ensureCheckout: %v", err)
	}

	// Verify promise.toml was extracted
	tomlPath := filepath.Join(checkoutDir, "promise.toml")
	if _, err := os.Stat(tomlPath); err != nil {
		t.Fatal("expected promise.toml in checkout")
	}

	// Verify .pr file was extracted
	prPath := filepath.Join(checkoutDir, "testmod.pr")
	if _, err := os.Stat(prPath); err != nil {
		t.Fatal("expected testmod.pr in checkout")
	}
}

// TestGitOpsUnderSafeBareRepositoryExplicit is the T0779 regression guard:
// Promise's own cache bare repos must stay fetchable/archivable even when git's
// safe.bareRepository=explicit hardening is in effect. Without the
// `-c safe.bareRepository=all` trust flag in runGit/runGitPiped, the cwd-based
// `git fetch` in ensureBareRepo is refused: "cannot use bare repository ...
// (safe.bareRepository is 'explicit')". The setting is forced on here so the
// guard is deterministic everywhere (CI does not normally set it).
func TestGitOpsUnderSafeBareRepositoryExplicit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	bareRepo, commitHash := createTestRepo(t, "hardened")

	// Force the hardening on for the production git calls below. Set AFTER the
	// fixture is built so only ensureBareRepo/ensureCheckout run under explicit.
	t.Setenv("GIT_CONFIG_PARAMETERS", "'safe.bareRepository=explicit'")

	cloneDir := filepath.Join(t.TempDir(), "clone.git")
	if err := ensureBareRepo(cloneDir, bareRepo); err != nil {
		t.Fatalf("ensureBareRepo (clone) under explicit: %v", err)
	}
	// Second call takes the fetch path — the operation that broke (T0779).
	if err := ensureBareRepo(cloneDir, bareRepo); err != nil {
		t.Fatalf("ensureBareRepo (fetch) under explicit: %v", err)
	}
	// ensureCheckout exercises the `git archive` (runGitPiped) path.
	checkoutDir := filepath.Join(t.TempDir(), "checkout")
	if err := ensureCheckout(cloneDir, checkoutDir, commitHash); err != nil {
		t.Fatalf("ensureCheckout under explicit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(checkoutDir, "promise.toml")); err != nil {
		t.Fatal("expected promise.toml in checkout under explicit")
	}
}

func TestResolveRemoteModuleLocalRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	bareRepo, commitHash := createTestRepo(t, "mylib")

	// Override global cache to a temp dir for testing
	origHome := os.Getenv("HOME")
	origPromiseHome := os.Getenv("PROMISE_HOME")
	tmpHome := t.TempDir()
	os.Unsetenv("PROMISE_HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)
	defer os.Setenv("PROMISE_HOME", origPromiseHome)

	// First resolve — should clone and checkout
	dir, err := ResolveRemoteModule(bareRepo, commitHash)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	// Verify the checkout has the expected files
	if _, err := os.Stat(filepath.Join(dir, "promise.toml")); err != nil {
		t.Fatal("expected promise.toml in resolved dir")
	}
	if _, err := os.Stat(filepath.Join(dir, "mylib.pr")); err != nil {
		t.Fatal("expected mylib.pr in resolved dir")
	}

	// Second resolve — should hit the fast path
	dir2, err := ResolveRemoteModule(bareRepo, commitHash)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if dir != dir2 {
		t.Errorf("second resolve returned different dir: %q vs %q", dir, dir2)
	}
}

func TestResolveRemoteModuleTwoCommits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Create a repo with two commits
	workDir := filepath.Join(t.TempDir(), "work")
	os.MkdirAll(workDir, 0755)

	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s\n%s", args, err, out)
		}
		return string(out)
	}

	run(workDir, "git", "init", "--initial-branch=main")
	run(workDir, "git", "config", "user.email", "test@test.com")
	run(workDir, "git", "config", "user.name", "Test")

	os.WriteFile(filepath.Join(workDir, "promise.toml"), []byte("[module]\nname = \"twocommit\"\nepoch = \"2026.0\"\n"), 0644)
	os.WriteFile(filepath.Join(workDir, "v1.pr"), []byte("v1() int `public { return 1; }\n"), 0644)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "v1")
	hash1 := run(workDir, "git", "rev-parse", "HEAD")
	hash1 = hash1[:len(hash1)-1]

	os.WriteFile(filepath.Join(workDir, "v2.pr"), []byte("v2() int `public { return 2; }\n"), 0644)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "v2")
	hash2 := run(workDir, "git", "rev-parse", "HEAD")
	hash2 = hash2[:len(hash2)-1]

	bareRepo := filepath.Join(t.TempDir(), "twocommit.git")
	run("", "git", "clone", "--bare", "--quiet", workDir, bareRepo)

	// Override HOME
	origHome := os.Getenv("HOME")
	origPromiseHome := os.Getenv("PROMISE_HOME")
	tmpHome := t.TempDir()
	os.Unsetenv("PROMISE_HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)
	defer os.Setenv("PROMISE_HOME", origPromiseHome)

	// Resolve first commit
	dir1, err := ResolveRemoteModule(bareRepo, hash1)
	if err != nil {
		t.Fatalf("resolve commit 1: %v", err)
	}

	// Resolve second commit
	dir2, err := ResolveRemoteModule(bareRepo, hash2)
	if err != nil {
		t.Fatalf("resolve commit 2: %v", err)
	}

	// Different directories
	if dir1 == dir2 {
		t.Error("expected different checkout dirs for different commits")
	}

	// v1 checkout should NOT have v2.pr
	if _, err := os.Stat(filepath.Join(dir1, "v2.pr")); err == nil {
		t.Error("v1 checkout should not have v2.pr")
	}

	// v2 checkout should have both
	if _, err := os.Stat(filepath.Join(dir2, "v1.pr")); err != nil {
		t.Error("v2 checkout should have v1.pr")
	}
	if _, err := os.Stat(filepath.Join(dir2, "v2.pr")); err != nil {
		t.Error("v2 checkout should have v2.pr")
	}
}

func TestPinResolveTag(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Create a repo with a tag
	workDir := filepath.Join(t.TempDir(), "work")
	os.MkdirAll(workDir, 0755)

	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s\n%s", args, err, out)
		}
		return string(out)
	}

	run(workDir, "git", "init", "--initial-branch=main")
	run(workDir, "git", "config", "user.email", "test@test.com")
	run(workDir, "git", "config", "user.name", "Test")
	os.WriteFile(filepath.Join(workDir, "promise.toml"), []byte("[module]\nname = \"tagged\"\nepoch = \"2026.0\"\n"), 0644)
	os.WriteFile(filepath.Join(workDir, "tagged.pr"), []byte("hello() int `public { return 1; }\n"), 0644)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "v1")
	run(workDir, "git", "tag", "v1.0.0")

	expectedHash := run(workDir, "git", "rev-parse", "HEAD")
	expectedHash = expectedHash[:len(expectedHash)-1]

	// PinResolve against the local repo using tag name
	resolved, err := PinResolve(workDir, "v1.0.0")
	if err != nil {
		t.Fatalf("PinResolve: %v", err)
	}

	if resolved != expectedHash {
		t.Errorf("PinResolve(v1.0.0) = %q, want %q", resolved, expectedHash)
	}
}

func TestPinResolveFullHash(t *testing.T) {
	hash := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	resolved, err := PinResolve("", hash)
	if err != nil {
		t.Fatalf("PinResolve: %v", err)
	}
	if resolved != hash {
		t.Errorf("PinResolve(%q) = %q, want same hash", hash, resolved)
	}
}

func TestPinResolveHEAD(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	workDir := filepath.Join(t.TempDir(), "work")
	os.MkdirAll(workDir, 0755)

	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s\n%s", args, err, out)
		}
		return string(out)
	}

	run(workDir, "git", "init", "--initial-branch=main")
	run(workDir, "git", "config", "user.email", "test@test.com")
	run(workDir, "git", "config", "user.name", "Test")
	os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("hello"), 0644)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "init")

	expectedHash := run(workDir, "git", "rev-parse", "HEAD")
	expectedHash = expectedHash[:len(expectedHash)-1]

	// Empty ref should resolve to HEAD
	resolved, err := PinResolve(workDir, "")
	if err != nil {
		t.Fatalf("PinResolve empty ref: %v", err)
	}
	if resolved != expectedHash {
		t.Errorf("PinResolve(\"\") = %q, want %q", resolved, expectedHash)
	}

	// Explicit "HEAD" should also resolve
	resolved2, err := PinResolve(workDir, "HEAD")
	if err != nil {
		t.Fatalf("PinResolve HEAD: %v", err)
	}
	if resolved2 != expectedHash {
		t.Errorf("PinResolve(\"HEAD\") = %q, want %q", resolved2, expectedHash)
	}
}

func TestPinResolveNotFound(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	workDir := filepath.Join(t.TempDir(), "work")
	os.MkdirAll(workDir, 0755)

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s\n%s", args, err, out)
		}
	}

	run(workDir, "git", "init", "--initial-branch=main")
	run(workDir, "git", "config", "user.email", "test@test.com")
	run(workDir, "git", "config", "user.name", "Test")
	os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("hello"), 0644)
	run(workDir, "git", "add", ".")
	run(workDir, "git", "commit", "-m", "init")

	_, err := PinResolve(workDir, "nonexistent-tag")
	if err == nil {
		t.Fatal("expected error for nonexistent ref")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestCleanGlobalCache(t *testing.T) {
	// Override HOME to a temp dir so we don't destroy real cache
	origHome := os.Getenv("HOME")
	origUserProfile := os.Getenv("USERPROFILE")
	origPromiseHome := os.Getenv("PROMISE_HOME")
	tmpHome := t.TempDir()
	os.Unsetenv("PROMISE_HOME")
	os.Setenv("HOME", tmpHome)
	if runtime.GOOS == "windows" {
		os.Setenv("USERPROFILE", tmpHome) // Windows uses USERPROFILE for home dir
	}
	defer os.Setenv("HOME", origHome)
	defer os.Setenv("USERPROFILE", origUserProfile)
	defer os.Setenv("PROMISE_HOME", origPromiseHome)

	// Create cache dir with some content
	cacheDir := filepath.Join(tmpHome, ".promise", "cache", "modules", "github.com", "someone", "parser")
	os.MkdirAll(cacheDir, 0755)
	os.WriteFile(filepath.Join(cacheDir, "test.txt"), []byte("data"), 0644)

	// Verify it exists
	modulesDir := filepath.Join(tmpHome, ".promise", "cache", "modules")
	if _, err := os.Stat(modulesDir); err != nil {
		t.Fatal("expected modules dir to exist before clean")
	}

	// Clean
	if err := CleanGlobalCache(); err != nil {
		t.Fatalf("CleanGlobalCache: %v", err)
	}

	// Should be gone
	if _, err := os.Stat(modulesDir); !os.IsNotExist(err) {
		t.Fatal("expected modules dir to be removed after clean")
	}
}

func TestCleanGlobalCacheNonexistent(t *testing.T) {
	// Should not error when cache doesn't exist
	origHome := os.Getenv("HOME")
	origUserProfile := os.Getenv("USERPROFILE")
	origPromiseHome := os.Getenv("PROMISE_HOME")
	tmpHome := t.TempDir()
	os.Unsetenv("PROMISE_HOME")
	os.Setenv("HOME", tmpHome)
	if runtime.GOOS == "windows" {
		os.Setenv("USERPROFILE", tmpHome)
	}
	defer os.Setenv("HOME", origHome)
	defer os.Setenv("USERPROFILE", origUserProfile)
	defer os.Setenv("PROMISE_HOME", origPromiseHome)

	if err := CleanGlobalCache(); err != nil {
		t.Fatalf("CleanGlobalCache on nonexistent dir: %v", err)
	}
}

func TestResolveRemoteModuleShortHash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local repo paths contain ':' which is invalid in Windows cache paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	bareRepo, commitHash := createTestRepo(t, "shorthash")

	origHome := os.Getenv("HOME")
	origPromiseHome := os.Getenv("PROMISE_HOME")
	tmpHome := t.TempDir()
	os.Unsetenv("PROMISE_HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)
	defer os.Setenv("PROMISE_HOME", origPromiseHome)

	// Use the full hash — checkout dir should use first 12 chars
	dir, err := ResolveRemoteModule(bareRepo, commitHash)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The checkout dir name should be the first 12 chars of the hash
	base := filepath.Base(dir)
	if len(base) != 12 {
		t.Errorf("expected checkout dir name to be 12 chars, got %d: %q", len(base), base)
	}
	if base != commitHash[:12] {
		t.Errorf("checkout dir = %q, want %q", base, commitHash[:12])
	}
}

func TestAcquireLockStaleLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".lock")

	// Create a stale lock file with an old modification time
	f, err := os.Create(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	// Set mtime to 10 minutes ago
	old := time.Now().Add(-10 * time.Minute)
	os.Chtimes(lockPath, old, old)

	// Should succeed — stale lock gets cleaned up
	unlock, err := acquireLock(lockPath)
	if err != nil {
		t.Fatalf("acquireLock with stale lock: %v", err)
	}
	unlock()
}

func TestAcquireLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".lock")

	// First lock should succeed
	unlock, err := acquireLock(lockPath)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	// Lock file should exist
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatal("expected lock file to exist")
	}

	// Release
	unlock()

	// Lock file should be removed
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatal("expected lock file to be removed after unlock")
	}

	// Second lock should succeed after unlock
	unlock2, err := acquireLock(lockPath)
	if err != nil {
		t.Fatalf("second lock: %v", err)
	}
	unlock2()
}
